package server

import (
	"context"
	"fmt"
	"time"

	"github.com/lni/goutils/syncutil"
	"go.uber.org/atomic"
)

type channelReactorSub struct {
	stopper *syncutil.Stopper

	channelQueue *channelList
	stopped      atomic.Bool

	advanceC     chan struct{}
	stepChannelC chan stepChannel
	r            *channelReactor
	index        int
}

func newChannelReactorSub(index int, r *channelReactor) *channelReactorSub {
	return &channelReactorSub{
		stopper:      syncutil.NewStopper(),
		channelQueue: newChannelList(),
		advanceC:     make(chan struct{}, 1),
		stepChannelC: make(chan stepChannel, 1024),
		r:            r,
		index:        index,
	}
}

func (r *channelReactorSub) start() error {
	r.stopper.RunWorker(r.loop)

	return nil
}

func (r *channelReactorSub) stop() {
	r.stopped.Store(true)
	r.stopper.Stop()
}

func (r *channelReactorSub) loop() {

	tk := time.NewTicker(1000 * time.Millisecond)

	for !r.stopped.Load() {
		r.readys()
		select {
		case <-tk.C:
			r.ticks()
		case <-r.advanceC:
		case req := <-r.stepChannelC:
			var err error
			if req.ch != nil {
				err = req.ch.step(req.action)
			}
			if req.waitC != nil {
				req.waitC <- err
			}
		case <-r.stopper.ShouldStop():
			return
		}
	}
}

func (r *channelReactorSub) step(ch *channel, action *ChannelAction) {
	select {
	case r.stepChannelC <- stepChannel{ch: ch, action: action}:
	case <-r.stopper.ShouldStop():
		return
	}
}

func (r *channelReactorSub) stepWait(ch *channel, action *ChannelAction) error {

	waitC := make(chan error, 1)
	select {
	case r.stepChannelC <- stepChannel{ch: ch, action: action, waitC: waitC}:
	case <-r.stopper.ShouldStop():
		return ErrReactorStopped
	}

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	select {
	case err := <-waitC:
		return err
	case <-timeoutCtx.Done():
		return timeoutCtx.Err()
	case <-r.stopper.ShouldStop():
		return ErrReactorStopped
	}
}

func (r *channelReactorSub) readys() {

	r.channelQueue.iter(func(ch *channel) {
		if r.stopped.Load() {
			return
		}
		if ch.hasReady() {
			r.handleReady(ch)
		}
	})
}

func (r *channelReactorSub) ticks() {
	r.channelQueue.iter(func(ch *channel) {
		if r.stopped.Load() {
			return
		}
		ch.tick()
	})
}

func (r *channelReactorSub) handleReady(ch *channel) {
	rd := ch.ready()

	for _, action := range rd.actions {
		fmt.Println("channel-action", action.ActionType, ch.channelId, ch.channelType)
		switch action.ActionType {
		case ChannelActionInit: // 初始化
			r.r.addInitReq(&initReq{
				ch: ch,
			})
		case ChannelActionPayloadDecrypt: // 消息解密
			r.r.addPayloadDecryptReq(&payloadDecryptReq{
				ch:       ch,
				messages: action.Messages,
			})
		case ChannelActionPermissionCheck: // 权限校验
			fromUid := action.Messages[0].FromUid
			r.r.addPermissionReq(&permissionReq{
				ch:       ch,
				fromUid:  fromUid,
				messages: action.Messages,
			})
		case ChannelActionStorage: // 消息存储
			r.r.addStorageReq(&storageReq{
				ch:       ch,
				messages: action.Messages,
			})
		case ChannelActionDeliver: // 消息投递
			r.r.addDeliverReq(&deliverReq{
				ch:          ch,
				channelId:   ch.channelId,
				channelType: ch.channelType,
				tagKey:      ch.receiverTagKey.Load(),
				messages:    action.Messages,
			})
		case ChannelActionSendack: // 发送回执
			r.r.addSendackReq(&sendackReq{
				ch:       ch,
				messages: action.Messages,
			})
		case ChannelActionForward: // 转发消息
			r.r.addForwardReq(&forwardReq{
				ch:       ch,
				messages: action.Messages,
				leaderId: action.LeaderId,
			})
		}
	}

}

func (r *channelReactorSub) channel(key string) *channel {
	return r.channelQueue.get(key)
}

func (r *channelReactorSub) addChannel(ch *channel) {
	r.channelQueue.add(ch)
}

func (r *channelReactorSub) advance() {
	select {
	case r.advanceC <- struct{}{}:
	default:
	}
}

type stepChannel struct {
	ch     *channel
	action *ChannelAction
	waitC  chan error
}
