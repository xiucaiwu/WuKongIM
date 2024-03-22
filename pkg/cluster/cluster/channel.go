package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/WuKongIM/WuKongIM/pkg/cluster/replica"
	"github.com/WuKongIM/WuKongIM/pkg/trace"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/WuKongIM/WuKongIM/pkg/wkstore"
	"github.com/sasha-s/go-deadlock"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type channel struct {
	channelID                  string
	channelType                uint8
	rc                         *replica.Replica              // 副本服务
	destroy                    bool                          // 是否已经销毁
	clusterConfig              *wkstore.ChannelClusterConfig // 分布式配置
	maxHandleReadyCountOfBatch int                           // 每批次处理ready的最大数量
	opts                       *Options
	lastActivity               atomic.Time // 最后一次活跃时间
	commitWait                 *commitWait
	traceRecord                *traceRecord
	doneC                      chan struct{}
	wklog.Log
	prev *channel
	next *channel

	mu                  deadlock.Mutex
	localstorage        *localStorage
	messageQueue        *ReplicaMessageQueue
	appendLogStoreQueue *localReplicaStoreQueue
	applyLogStoreQueue  *localReplicaStoreQueue
}

func newChannel(clusterConfig *wkstore.ChannelClusterConfig, appliedIdx uint64, localstorage *localStorage, opts *Options) *channel {
	shardNo := ChannelKey(clusterConfig.ChannelID, clusterConfig.ChannelType)
	rc := replica.New(opts.NodeID, shardNo, replica.WithAppliedIndex(appliedIdx), replica.WithReplicas(clusterConfig.Replicas), replica.WithStorage(newProxyReplicaStorage(shardNo, opts.MessageLogStorage, localstorage)))
	ch := &channel{
		maxHandleReadyCountOfBatch: 50,
		rc:                         rc,
		opts:                       opts,
		Log:                        wklog.NewWKLog(fmt.Sprintf("Channel[%s]", shardNo)),
		commitWait:                 newCommitWait(),
		channelID:                  clusterConfig.ChannelID,
		channelType:                clusterConfig.ChannelType,
		clusterConfig:              clusterConfig,
		doneC:                      make(chan struct{}),
		localstorage:               localstorage,
		traceRecord:                newTraceRecord(),
		appendLogStoreQueue:        newLcalReplicaStoreQueue(),
		applyLogStoreQueue:         newLcalReplicaStoreQueue(),
		messageQueue:               NewReplicaMessageQueue(opts.ReceiveQueueLength, false, opts.LazyFreeCycle, opts.MaxReceiveQueueSize),
	}
	ch.lastActivity.Store(time.Now())
	return ch
}

func (c *channel) updateClusterConfig(clusterConfig *wkstore.ChannelClusterConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusterConfig = clusterConfig
	c.rc.SetReplicas(clusterConfig.Replicas)
	if clusterConfig.LeaderId == c.opts.NodeID {
		c.rc.BecomeLeader(clusterConfig.Term)
	} else {
		c.rc.BecomeFollower(clusterConfig.Term, clusterConfig.LeaderId)
	}
}

func (c *channel) updateClusterConfigLeaderId(leaderId uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusterConfig.LeaderId = leaderId
}

func (c *channel) updateClusterConfigLeaderIdAndTerm(term uint32, leaderId uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clusterConfig.Term = term
	c.clusterConfig.LeaderId = leaderId
}

func (c *channel) ready() replica.Ready {
	if c.destroy {
		return replica.Ready{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rc.Ready()
}

func (c *channel) hasReady() bool {
	if c.destroy {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rc.HasReady()
}

// 任命为领导
func (c *channel) appointLeader(term uint32) error {

	return c.stepLock(replica.Message{
		MsgType:           replica.MsgAppointLeaderReq,
		AppointmentLeader: c.opts.NodeID,
		Term:              term,
	})

}

// 任命指定节点为领导
func (c *channel) appointLeaderTo(term uint32, to uint64) error {
	return c.stepLock(replica.Message{
		MsgType:           replica.MsgAppointLeaderReq,
		AppointmentLeader: to,
		Term:              term,
	})
}

func (c *channel) stepLock(msg replica.Message) error {
	c.mu.Lock()
	err := c.step(msg)
	c.mu.Unlock()
	return err

}

func (c *channel) step(msg replica.Message) error {
	if c.destroy {
		return errors.New("channel destroy, can not step")
	}
	c.lastActivity.Store(time.Now())
	return c.rc.Step(msg)
}

func (c *channel) propose(data []byte) error {
	if c.destroy {
		return errors.New("channel destroy, can not propose")
	}
	return c.stepLock(c.rc.NewProposeMessage(data))
}

func (c *channel) proposeAndWaitCommit(ctx context.Context, data []byte, timeout time.Duration) (uint64, error) {
	if c.destroy {
		return 0, errors.New("channel destroy, can not propose")
	}
	lastIndexs, err := c.proposeAndWaitCommits(ctx, [][]byte{data}, timeout)
	if err != nil {
		return 0, err
	}
	if len(lastIndexs) == 0 {
		return 0, errors.New("lastIndexs is empty")
	}
	return lastIndexs[0], nil
}

func (c *channel) becomeLeader(term uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rc.BecomeLeader(term)
}

// 提案数据，并等待数据提交给大多数节点
func (c *channel) proposeAndWaitCommits(ctx context.Context, data [][]byte, timeout time.Duration) ([]uint64, error) {
	if len(data) == 0 {
		return nil, errors.New("data is empty")
	}
	c.mu.Lock()
	if c.destroy {
		c.mu.Unlock()
		return nil, errors.New("channel destroy, can not propose")
	}

	parentCtx, parentSpan := trace.GlobalTrace.StartSpan(ctx, "proposeAndCommitLogs")
	defer parentSpan.End()
	parentSpan.SetUint32("term", c.rc.Term())
	logs := make([]replica.Log, 0, len(data))
	for i, d := range data {
		logs = append(logs,
			replica.Log{
				Index: c.rc.LastLogIndex() + uint64(1+i),
				Term:  c.rc.Term(),
				Data:  d,
			},
		)
	}
	firstLog := logs[0]
	lastLog := logs[len(logs)-1]
	c.Debug("add wait index", zap.Uint64("lastLogIndex", lastLog.Index), zap.Int("logsCount", len(logs)))
	waitC, err := c.commitWait.addWaitIndex(lastLog.Index)
	if err != nil {
		c.mu.Unlock()
		parentSpan.RecordError(err)
		c.Error("add wait index failed", zap.Error(err))
		return nil, err
	}
	parentSpan.SetUint64("firstLogIndex", firstLog.Index)
	parentSpan.SetUint64("lastLogIndex", lastLog.Index)
	parentSpan.SetUint64("waitLogIndex", lastLog.Index)
	parentSpan.SetInt("logCount", len(logs))

	// 记录追踪index的范围
	c.traceRecord.addProposeSpanRange(firstLog.Index, lastLog.Index, parentSpan, parentCtx)

	defer func() {
		c.traceRecord.removeProposeSpanWithRange(firstLog.Index, lastLog.Index)
	}()

	_, proposeLogSpan := trace.GlobalTrace.StartSpan(parentCtx, "proposeLogs")
	err = c.step(c.rc.NewProposeMessageWithLogs(logs))
	if err != nil {
		proposeLogSpan.RecordError(err)
		parentSpan.RecordError(err)
		proposeLogSpan.End()
		c.mu.Unlock()
		return nil, err
	}
	proposeLogSpan.End()
	c.mu.Unlock()

	timeoutCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case <-waitC:
		seqs := make([]uint64, 0, len(logs))
		for _, log := range logs {
			seqs = append(seqs, log.Index)
		}
		c.Debug("finsh wait index", zap.Uint64s("seqs", seqs))
		return seqs, nil
	case <-timeoutCtx.Done():
		c.Debug("proposeAndWaitCommits timeout", zap.Uint64("lastLogIndex", lastLog.Index), zap.Int("logCount", len(logs)))
		parentSpan.RecordError(timeoutCtx.Err())
		return nil, timeoutCtx.Err()
	case <-c.doneC:
		parentSpan.RecordError(ErrStopped)
		return nil, ErrStopped
	}
}

func (c *channel) channelKey() string {
	return ChannelKey(c.channelID, c.channelType)
}

func (c *channel) makeDestroy() {
	c.destroy = true
	close(c.doneC)
}

func (c *channel) isDestroy() bool {
	return c.destroy
}

func (c *channel) getLastActivity() time.Time {
	return c.lastActivity.Load()
}

func (c *channel) handleLocalMsg(msg replica.Message) {
	if c.destroy {
		c.Warn("handle local msg, but channel is destroy")
		return
	}
	if msg.To != c.opts.NodeID {
		c.Warn("handle local msg, but msg to is not self", zap.String("msgType", msg.MsgType.String()), zap.Uint64("to", msg.To), zap.Uint64("self", c.opts.NodeID))
		return
	}
	c.lastActivity.Store(time.Now())
	switch msg.MsgType {
	case replica.MsgStoreAppend: // 处理store append请求
		c.handleStoreAppend(msg)
	case replica.MsgApplyLogsReq: // 处理apply logs请求
		c.handleApplyLogsReq(msg)
	}
}

func (c *channel) handleStoreAppend(msg replica.Message) {
	if len(msg.Logs) == 0 {
		return
	}

	lastLog := msg.Logs[len(msg.Logs)-1]

	c.appendLogStoreQueue.add(c.rc.NewMsgStoreAppendResp(lastLog.Index))

	go c.appendLogs(msg)

}

func (c *channel) appendLogs(msg replica.Message) {
	shardNo := ChannelKey(c.channelID, c.channelType)

	firstLog := msg.Logs[0]
	lastLog := msg.Logs[len(msg.Logs)-1]

	ctxs := c.traceRecord.getProposeContextsWithRange(firstLog.Index, lastLog.Index)
	for _, parentCtx := range ctxs {
		_, span := trace.GlobalTrace.StartSpan(parentCtx, fmt.Sprintf("logsAppend[node %d]", c.opts.NodeID))
		defer func(sp trace.Span, pCtx context.Context) {
			sp.End()
			commitSpanCtx, commitSpan := trace.GlobalTrace.StartSpan(pCtx, fmt.Sprintf("logsCommit[node %d]", c.opts.NodeID))
			commitSpan.SetUint64("firstLogIndex", firstLog.Index)
			commitSpan.SetUint64("lastLogIndex", lastLog.Index)
			c.traceRecord.addCommitSpanRange(firstLog.Index, lastLog.Index, commitSpan, commitSpanCtx)
		}(span, parentCtx)

		span.SetInt("logCount", len(msg.Logs))
		span.SetUint64("firstLogIndex", firstLog.Index)
		span.SetUint64("lastLogIndex", lastLog.Index)

	}

	start := time.Now()

	c.Debug("append log", zap.Uint64("lastLogIndex", lastLog.Index))
	err := c.opts.MessageLogStorage.AppendLog(shardNo, msg.Logs)
	if err != nil {
		c.Panic("append log error", zap.Error(err))
	}
	c.Debug("append log done", zap.Uint64("lastLogIndex", lastLog.Index), zap.Duration("cost", time.Since(start)))

	if set := c.appendLogStoreQueue.setStored(lastLog.Index); !set {
		c.Panic("set stored failed", zap.Uint64("lastLogIndex", lastLog.Index))
	}
}

// 处理应用日志请求
func (c *channel) handleApplyLogsReq(msg replica.Message) {
	if msg.CommittedIndex <= 0 || msg.AppliedIndex >= msg.CommittedIndex {
		return
	}

	c.applyLogStoreQueue.add(c.rc.NewMsgApplyLogsRespMessage(msg.CommittedIndex))

	go c.applyLogs(msg)

}

func (c *channel) applyLogs(msg replica.Message) {
	spans := c.traceRecord.getCommitSpanWithRange(msg.AppliedIndex, msg.CommittedIndex)

	if len(spans) > 0 {
		c.traceRecord.removeCommitSpanWithRange(msg.AppliedIndex, msg.CommittedIndex)
		for _, span := range spans {
			span.SetUint64("appliedIndex", msg.AppliedIndex)
			span.SetUint64("committedIndex", msg.CommittedIndex)
			span.SetUint64("lastLogIndex", c.rc.LastLogIndex())
			span.End()
		}
	}

	start := time.Now()
	c.Debug("commit wait", zap.Uint64("committedIndex", msg.CommittedIndex))
	c.commitWait.commitIndex(msg.CommittedIndex)
	c.Debug("commit wait done", zap.Duration("cost", time.Since(start)), zap.Uint64("committedIndex", msg.CommittedIndex))

	shardNo := ChannelKey(c.channelID, c.channelType)
	err := c.localstorage.setAppliedIndex(shardNo, msg.CommittedIndex)
	if err != nil {
		c.Error("set applied index failed", zap.Error(err))
		return
	}

	if set := c.applyLogStoreQueue.setStored(msg.CommittedIndex); !set {
		c.Panic("applyLogs: set stored failed", zap.Uint64("committedIndex", msg.CommittedIndex))
	}
}

func (c *channel) handleMessage(msg replica.Message) error {
	if c.destroy {
		return errors.New("channel destroy, can not handleMessage")
	}
	c.lastActivity.Store(time.Now())

	if msg.MsgType == replica.MsgSync { // 领导收到副本的同步请求
		// c.Debug("sync logs", zap.Uint64("index", msg.Index))
		preSyncSpans := c.traceRecord.getSyncSpanWithIndex(msg.From, msg.Index)
		for _, preSyncSpan := range preSyncSpans {
			preSyncSpan.span.SetUint64("endSyncIndex", msg.Index)
			preSyncSpan.span.End()
			c.traceRecord.removeSyncSpanWithIndex(msg.From, preSyncSpan.startIndex)
		}
		// ctxs := c.commitWait.spanContexts(c.LastLogIndex())
		commitCtxs := c.traceRecord.getCommitContextsWithRange(msg.CommittedIndex, c.rc.LastLogIndex())
		for _, commitCtx := range commitCtxs {
			syncSpanCtx, syncSpan := trace.GlobalTrace.StartSpan(commitCtx, fmt.Sprintf("logsSync[from %d]", msg.From))
			syncSpan.SetUint64("from", msg.From)
			syncSpan.SetUint32("term", msg.Term)
			syncSpan.SetUint64("startSyncIndex", msg.Index)
			syncSpan.SetUint64("lastLogIndex", c.rc.LastLogIndex())

			c.traceRecord.addSyncSpan(msg.From, msg.Index, syncSpan, syncSpanCtx)

		}

		proposeCtxs := c.traceRecord.getProposeContextsWithRange(msg.CommittedIndex, c.rc.LastLogIndex())
		var traceIDs [][16]byte // 追踪ID
		var spanIDs [][8]byte   // 跨度ID
		for _, proposeCtx := range proposeCtxs {
			parentSpan := trace.SpanFromContext(proposeCtx)
			traceIDs = append(traceIDs, parentSpan.SpanContext().TraceID())
			spanIDs = append(spanIDs, parentSpan.SpanContext().SpanID())

			msg.TraceIDs = traceIDs
			msg.SpanIDs = spanIDs
		}
	} else if msg.MsgType == replica.MsgSyncResp { // 副本收到领导的同步响应
		if len(msg.TraceIDs) > 0 {
			for i, traceID := range msg.TraceIDs {
				spanID := msg.SpanIDs[i]
				spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
					TraceID: traceID,
					SpanID:  spanID,
					Remote:  true,
				})
				firstIndex := msg.Index
				lastIndex := c.LastLogIndex()
				if len(msg.Logs) > 0 {
					lastIndex = msg.Logs[len(msg.Logs)-1].Index
				}
				ctx := trace.ContextWithRemoteSpanContext(context.Background(), spanCtx)
				c.traceRecord.addProposeSpanRange(firstIndex, lastIndex, nil, ctx)

				// _, span := trace.GlobalTrace.StartSpan(ctx, "logsSyncResp")
				// defer span.End()
				// if span != nil {
				// 	span.SetUint64("syncIndex", msg.Index)
				// 	span.SetUint64("lastLogIndex", c.rc.LastLogIndex())
				// 	span.SetUint64("from", msg.From)
				// 	span.SetUint32("term", msg.Term)
				// 	span.SetInt("logCount", len(msg.Logs))
				// }
			}

		}
	}

	if added, stopped := c.messageQueue.Add(msg); !added || stopped {
		c.Error("messageQueue add failed")
		return errors.New("messageQueue add failed")
	}
	return nil
}

func (c *channel) handleReceivedMessages() error {
	if c.destroy {
		return errors.New("channel destroy, can not handle message")
	}
	msgs := c.messageQueue.Get()
	var err error
	for _, msg := range msgs {
		err = c.stepLock(msg)
		if err != nil {
			c.Error("step message failed", zap.Error(err))
			return err
		}
	}
	return nil
}

func (c *channel) handleLocalStoreMsgs() error {
	if c.destroy {
		return errors.New("channel destroy, can not handle message")
	}
	for c.appendLogStoreQueue.firstIsStored() {
		msg, ok := c.appendLogStoreQueue.removeFirst()
		if !ok {
			break
		}
		err := c.stepLock(msg.Message)
		if err != nil {
			c.Panic("step local store message failed", zap.Error(err))
			return err
		}
	}

	for c.applyLogStoreQueue.firstIsStored() {
		msg, ok := c.applyLogStoreQueue.removeFirst()
		if !ok {
			break
		}
		err := c.stepLock(msg.Message)
		if err != nil {
			c.Panic("applyLogStoreQueue: step local store message failed", zap.Error(err))
			return err
		}
	}

	return nil
}

func (c *channel) LastLogIndex() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rc.LastLogIndex()
}

func (c *channel) IsLeader() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rc.IsLeader()
}

func (c *channel) LeaderId() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rc.LeaderId()
}

func (c *channel) getClusterConfig() *wkstore.ChannelClusterConfig {
	return c.clusterConfig
}

type ichannel interface {
	IsLeader() bool
	proposeAndWaitCommits(ctx context.Context, data [][]byte, timeout time.Duration) ([]uint64, error)
	LeaderId() uint64
	handleMessage(msg replica.Message) error
	getClusterConfig() *wkstore.ChannelClusterConfig
}

type proxyChannel struct {
	nodeId     uint64
	clusterCfg *wkstore.ChannelClusterConfig
	mu         sync.Mutex
}

func newProxyChannel(nodeId uint64, clusterCfg *wkstore.ChannelClusterConfig) *proxyChannel {
	return &proxyChannel{
		nodeId:     nodeId,
		clusterCfg: clusterCfg,
	}
}

func (p *proxyChannel) IsLeader() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clusterCfg.LeaderId == p.nodeId
}

func (p *proxyChannel) proposeAndWaitCommits(ctx context.Context, data [][]byte, timeout time.Duration) ([]uint64, error) {
	panic("proposeAndWaitCommits: implement me")
}

func (p *proxyChannel) LeaderId() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clusterCfg.LeaderId
}

func (p *proxyChannel) handleMessage(msg replica.Message) error {
	panic("handleMessage: implement me")
}

func (p *proxyChannel) getClusterConfig() *wkstore.ChannelClusterConfig {
	return p.clusterCfg
}
