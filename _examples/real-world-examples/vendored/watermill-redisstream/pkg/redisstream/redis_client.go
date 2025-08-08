package redisstream

import (
	"context"

	"github.com/on-the-ground/effect_ive_go/effects/dependency"
	"github.com/redis/go-redis/v9"
)

type _redisClient interface {
	redis.StreamCmdable
	Close() error
}

// XGroupCreateMkStream(ctx context.Context, stream, group, start string) *StatusCmd
// XReadGroup(ctx context.Context, a *XReadGroupArgs) *XStreamSliceCmd
// XRead(ctx context.Context, a *XReadArgs) *XStreamSliceCmd
//	XPendingExt(ctx context.Context, a *XPendingExtArgs) *XPendingExtCmd
// 	XAck(ctx context.Context, stream, group string, ids ...string) *IntCmd
// 	XClaim(ctx context.Context, a *XClaimArgs) *XMessageSliceCmd
// 	XInfoConsumers(ctx context.Context, key string, group string) *XInfoConsumersCmd
//	XGroupDelConsumer(ctx context.Context, stream, group, consumer string) *IntCmd

type XGroupDelConsumerEffect struct {
	dependency              _redisClient
	ctx                     context.Context
	stream, group, consumer string
}

func (i XGroupDelConsumerEffect) WithReceiver(receiver any) dependency.Quacker {
	i.dependency = receiver.(_redisClient)
	return i
}

func (i XGroupDelConsumerEffect) Quack(ctx context.Context) (any, error) {
	return nil, i.dependency.XGroupDelConsumer(
		i.ctx,
		i.stream,
		i.group,
		i.consumer,
	).Err()
}

type XAckEffect struct {
	dependency    _redisClient
	ctx           context.Context
	stream, group string
	ids           []string
}

func (i XAckEffect) WithReceiver(receiver any) dependency.Quacker {
	i.dependency = receiver.(_redisClient)
	return i
}

func (i XAckEffect) Quack(ctx context.Context) (any, error) {
	return nil, i.dependency.XAck(
		i.ctx,
		i.stream,
		i.group,
		i.ids...,
	).Err()
}
