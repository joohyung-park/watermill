package redisstream

import (
	"context"
	"sync"
	"time"

	"github.com/Rican7/retry"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/on-the-ground/effect_ive_go/effects/dependency"
	"github.com/on-the-ground/effect_ive_go/effects/log"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
)

const (
	groupStartid   = ">"
	redisBusyGroup = "BUSYGROUP Consumer Group name already exists"
)

const (
	// NoSleep can be set to SubscriberConfig.NackResendSleep
	NoSleep time.Duration = -1

	DefaultBlockTime = time.Millisecond * 100

	DefaultClaimInterval = time.Second * 5

	DefaultClaimBatchSize = int64(100)

	DefaultMaxIdleTime = time.Second * 60

	DefaultCheckConsumersInterval = time.Second * 300
	DefaultConsumerTimeout        = time.Second * 600
)

type Subscriber struct {
	config        SubscriberConfig
	closing       chan struct{}
	subscribersWg sync.WaitGroup

	closed     bool
	closeMutex sync.Mutex
}

// NewSubscriber creates a new redis stream Subscriber.
func NewSubscriber(config SubscriberConfig, logger watermill.LoggerAdapter) (*Subscriber, error) {
	config.setDefaults()

	if err := config.Validate(); err != nil {
		return nil, err
	}

	if logger == nil {
		logger = &watermill.NopLogger{}
	}

	return &Subscriber{
		config: config,

		closing: make(chan struct{}),
	}, nil
}

type SubscriberConfig struct {
	Unmarshaller Unmarshaller

	// Redis stream consumer id, paired with ConsumerGroup.
	Consumer string
	// When empty, fan-out mode will be used.
	ConsumerGroup string

	// How long after Nack message should be redelivered.
	NackResendSleep time.Duration

	// Block to wait next redis stream message.
	BlockTime time.Duration

	// Claim idle pending message interval.
	ClaimInterval time.Duration

	// How many pending messages are claimed at most each claim interval.
	ClaimBatchSize int64

	// How long should we treat a pending message as claimable.
	MaxIdleTime time.Duration

	// Check consumer status interval.
	CheckConsumersInterval time.Duration

	// After this timeout an idle consumer with no pending messages will be removed from the consumer group.
	ConsumerTimeout time.Duration

	// Start consumption from the specified message ID.
	// When using "0", the consumer group will consume from the very first message.
	// When using "$", the consumer group will consume from the latest message.
	OldestId string

	// If consumer group in not set, for fanout start consumption from the specified message ID.
	// When using "0", the consumer will consume from the very first message.
	// When using "$", the consumer will consume from the latest message.
	FanOutOldestId string

	// If this is set, it will be called to decide whether a pending message that
	// has been idle for more than MaxIdleTime should actually be claimed.
	// If this is not set, then all pending messages that have been idle for more than MaxIdleTime will be claimed.
	// This can be useful e.g. for tasks where the processing time can be very variable -
	// so we can't just use a short MaxIdleTime; but at the same time dead
	// consumers should be spotted quickly - so we can't just use a long MaxIdleTime either.
	// In such cases, if we have another way for checking consumers' health, then we can
	// leverage that in this callback.
	ShouldClaimPendingMessage func(redis.XPendingExt) bool

	// If this is set, it will be called to decide whether a reading error
	// should return the read method and close the subscriber or just log the error
	// and continue.
	ShouldStopOnReadErrors func(error) bool
}

func (sc *SubscriberConfig) setDefaults() {
	if sc.Unmarshaller == nil {
		sc.Unmarshaller = DefaultMarshallerUnmarshaller{}
	}
	if sc.Consumer == "" {
		sc.Consumer = watermill.NewShortUUID()
	}
	if sc.NackResendSleep == 0 {
		sc.NackResendSleep = NoSleep
	}
	if sc.BlockTime == 0 {
		sc.BlockTime = DefaultBlockTime
	}
	if sc.ClaimInterval == 0 {
		sc.ClaimInterval = DefaultClaimInterval
	}
	if sc.ClaimBatchSize == 0 {
		sc.ClaimBatchSize = DefaultClaimBatchSize
	}
	if sc.MaxIdleTime == 0 {
		sc.MaxIdleTime = DefaultMaxIdleTime
	}
	if sc.CheckConsumersInterval == 0 {
		sc.CheckConsumersInterval = DefaultCheckConsumersInterval
	}
	if sc.ConsumerTimeout == 0 {
		sc.ConsumerTimeout = DefaultConsumerTimeout
	}
	// Consume from scratch by default
	if sc.OldestId == "" {
		sc.OldestId = "0"
	}

	if sc.FanOutOldestId == "" {
		sc.FanOutOldestId = "$"
	}
}

func (sc *SubscriberConfig) Validate() error {
	return nil
}

func (s *Subscriber) Subscribe(ctx context.Context, topic string) (<-chan *message.Message, error) {
	if s.closed {
		return nil, errors.New("subscriber closed")
	}

	s.subscribersWg.Add(1)

	logFields := watermill.LogFields{
		"provider":       "redis",
		"topic":          topic,
		"consumer_group": s.config.ConsumerGroup,
		"consumer_uuid":  s.config.Consumer,
	}
	log.Effect(ctx, log.LogInfo, "Subscribing to redis stream topic", logFields)

	// we don't want to have buffered channel to not consume messsage from redis stream when consumer is not consuming
	output := make(chan *message.Message)

	consumeClosed, err := s.consumeMessages(ctx, topic, output, logFields)
	if err != nil {
		s.subscribersWg.Done()
		return nil, err
	}

	go func() {
		<-consumeClosed
		close(output)
		s.subscribersWg.Done()
	}()

	return output, nil
}

func (s *Subscriber) consumeMessages(ctx context.Context, topic string, output chan *message.Message, logFields watermill.LogFields) (consumeMessageClosed chan struct{}, err error) {
	log.Effect(ctx, log.LogInfo, "Starting consuming", logFields)

	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-s.closing:
			log.Effect(ctx, log.LogDebug, "Closing subscriber, cancelling consumeMessages", logFields)
			cancel()
		case <-ctx.Done():
			// avoid goroutine leak
		}
	}()
	if s.config.ConsumerGroup != "" {
		// create consumer group
		if _, err := s.client.XGroupCreateMkStream(ctx, topic, s.config.ConsumerGroup, s.config.OldestId).Result(); err != nil && err.Error() != redisBusyGroup {
			return nil, err
		}
	}

	consumeMessageClosed, err = s.consumeStreams(ctx, topic, output, logFields)
	if err != nil {
		log.Effect(ctx, log.LogDebug,
			"Starting consume failed, cancelling context",
			logFields.Add(watermill.LogFields{"err": err}),
		)
		cancel()
		return nil, err
	}

	return consumeMessageClosed, nil
}

func (s *Subscriber) consumeStreams(ctx context.Context, stream string, output chan *message.Message, logFields watermill.LogFields) (chan struct{}, error) {
	messageHandler := s.createMessageHandler(output)
	consumeMessageClosed := make(chan struct{})

	go func() {
		defer close(consumeMessageClosed)

		readChannel := make(chan *redis.XStream, 1)
		go s.read(ctx, stream, readChannel, logFields)

		for {
			select {
			case xs := <-readChannel:
				if xs == nil {
					log.Effect(ctx, log.LogDebug, "readStreamChannel is closed, stopping readStream", logFields)
					return
				}
				if err := messageHandler.processMessage(ctx, xs.Stream, &xs.Messages[0], logFields); err != nil {
					log.Effect(ctx, log.LogError, "processMessage fail", watermill.LogFields{"error": err}.Add(logFields))
					return
				}
			case <-s.closing:
				log.Effect(ctx, log.LogDebug, "Subscriber is closing, stopping readStream", logFields)
				return
			case <-ctx.Done():
				log.Effect(ctx, log.LogDebug, "Ctx was cancelled, stopping readStream", logFields)
				return
			}
		}
	}()

	return consumeMessageClosed, nil
}

func (s *Subscriber) read(ctx context.Context, stream string, readChannel chan<- *redis.XStream, logFields watermill.LogFields) {
	wg := &sync.WaitGroup{}
	subCtx, subCancel := context.WithCancel(ctx)
	defer func() {
		subCancel()
		wg.Wait()
		close(readChannel)
	}()
	var (
		streamsGroup = []string{stream, groupStartid}

		fanOutStartid               = s.config.FanOutOldestId
		countFanOut   int64         = 0
		blockTime     time.Duration = 0

		xss []redis.XStream
		xs  *redis.XStream
		err error
	)

	if s.config.ConsumerGroup != "" {
		// 1. get pending message from idle consumer
		wg.Add(1)
		s.claim(subCtx, stream, readChannel, false, wg, logFields)

		// 2. background
		wg.Add(1)
		go s.claim(subCtx, stream, readChannel, true, wg, logFields)

		// check consumer status and remove idling consumers if possible
		wg.Add(1)
		go s.checkConsumers(subCtx, stream, wg, logFields)
	}

	for {
		select {
		case <-s.closing:
			return
		case <-ctx.Done():
			return
		default:
			if s.config.ConsumerGroup != "" {
				xss, err = s.client.XReadGroup(
					ctx,
					&redis.XReadGroupArgs{
						Group:    s.config.ConsumerGroup,
						Consumer: s.config.Consumer,
						Streams:  streamsGroup,
						Count:    1,
						Block:    blockTime,
					}).Result()
			} else {
				xss, err = s.client.XRead(
					ctx,
					&redis.XReadArgs{
						Streams: []string{stream, fanOutStartid},
						Count:   countFanOut,
						Block:   blockTime,
					}).Result()
			}
			if err == redis.Nil {
				continue
			} else if err != nil {
				if s.config.ShouldStopOnReadErrors != nil {
					if s.config.ShouldStopOnReadErrors(err) {
						log.Effect(ctx, log.LogError, "stop reading after error", watermill.LogFields{"error": err}.Add(logFields))
						return
					}
				}
				// prevent excessive output from abnormal connections
				time.Sleep(500 * time.Millisecond)
				log.Effect(ctx, log.LogError, "read fail", watermill.LogFields{"error": err}.Add(logFields))
			}
			if len(xss) < 1 || len(xss[0].Messages) < 1 {
				continue
			}
			// update last delivered message
			xs = &xss[0]
			if s.config.ConsumerGroup == "" {
				fanOutStartid = xs.Messages[0].ID
				countFanOut = 1
			}

			blockTime = s.config.BlockTime

			select {
			case <-s.closing:
				return
			case <-ctx.Done():
				return
			case readChannel <- xs:
			}
		}
	}
}

func (s *Subscriber) claim(ctx context.Context, stream string, readChannel chan<- *redis.XStream, keep bool, wg *sync.WaitGroup, logFields watermill.LogFields) {
	var (
		xps    []redis.XPendingExt
		err    error
		xp     redis.XPendingExt
		xm     []redis.XMessage
		tick   = time.NewTicker(s.config.ClaimInterval)
		initCh = make(chan byte, 1)
	)
	defer func() {
		tick.Stop()
		close(initCh)
		wg.Done()
	}()
	if !keep { // if not keep, run immediately
		initCh <- 1
	}

OUTER_LOOP:
	for {
		select {
		case <-s.closing:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-initCh:
		}

		xps, err = s.client.XPendingExt(ctx, &redis.XPendingExtArgs{
			Stream: stream,
			Group:  s.config.ConsumerGroup,
			Idle:   s.config.MaxIdleTime,
			Start:  "0",
			End:    "+",
			Count:  s.config.ClaimBatchSize,
		}).Result()
		if err != nil {
			log.Effect(ctx, log.LogError,
				"xpendingext fail",
				watermill.LogFields{"error": err}.Add(logFields),
			)
			continue
		}
		for _, xp = range xps {
			shouldClaim := xp.Idle >= s.config.MaxIdleTime
			if shouldClaim && s.config.ShouldClaimPendingMessage != nil {
				shouldClaim = s.config.ShouldClaimPendingMessage(xp)
			}

			if shouldClaim {
				// assign the ownership of a pending message to the current consumer
				xm, err = s.client.XClaim(ctx, &redis.XClaimArgs{
					Stream:   stream,
					Group:    s.config.ConsumerGroup,
					Consumer: s.config.Consumer,
					// this is important: it ensures that 2 concurrent subscribers
					// won't claim the same pending message at the same time
					MinIdle:  s.config.MaxIdleTime,
					Messages: []string{xp.ID},
				}).Result()
				if err == redis.Nil {
					// Any messages that are nil, should be xacked and skipped
					<-dependency.Effect[_redisClient](ctx, XAckEffect{
						ctx:    ctx,
						stream: stream,
						group:  s.config.ConsumerGroup,
						ids:    []string{xp.ID},
					})

					continue
				} else if err != nil {
					log.Effect(ctx, log.LogError,
						"xclaim fail",
						watermill.LogFields{"error": err}.
							Add(logFields).
							Add(watermill.LogFields{"xp": xp}),
					)
					continue OUTER_LOOP
				}
				if len(xm) > 0 {
					select {
					case <-s.closing:
						return
					case <-ctx.Done():
						return
					case readChannel <- &redis.XStream{Stream: stream, Messages: xm}:
					}
				}
			}
		}
		if len(xps) == 0 || int64(len(xps)) < s.config.ClaimBatchSize { // done
			if !keep {
				return
			}
			continue
		}
	}
}

func (s *Subscriber) checkConsumers(ctx context.Context, stream string, wg *sync.WaitGroup, logFields watermill.LogFields) {
	tick := time.NewTicker(s.config.CheckConsumersInterval)
	defer func() {
		tick.Stop()
		wg.Done()
	}()

	for {
		select {
		case <-s.closing:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		xics, err := s.client.XInfoConsumers(ctx, stream, s.config.ConsumerGroup).Result()
		if err != nil {
			log.Effect(ctx, log.LogError,
				"xinfoconsumers failed",
				watermill.LogFields{"error": err}.Add(logFields),
			)
		}
		for _, xic := range xics {
			if xic.Idle < s.config.ConsumerTimeout || xic.Pending != 0 {
				continue
			}
			if res := <-dependency.Effect[_redisClient](ctx, XGroupDelConsumerEffect{
				ctx:      ctx,
				stream:   stream,
				group:    s.config.ConsumerGroup,
				consumer: xic.Name,
			}); res.Err != nil {
				log.Effect(ctx, log.LogError,
					"xgroupdelconsumer failed",
					watermill.LogFields{"error": err}.Add(logFields),
				)
			}
		}
	}
}

func (s *Subscriber) createMessageHandler(output chan *message.Message) messageHandler {
	return messageHandler{
		outputChannel:   output,
		consumerGroup:   s.config.ConsumerGroup,
		unmarshaller:    s.config.Unmarshaller,
		nackResendSleep: s.config.NackResendSleep,
		closing:         s.closing,
	}
}

func (s *Subscriber) Close() error {
	s.closeMutex.Lock()
	defer s.closeMutex.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true
	close(s.closing)
	s.subscribersWg.Wait()

	if err := s.client.Close(); err != nil {
		return err
	}

	log.Effect(ctx, log.LogDebug, "Redis stream subscriber closed", nil)

	return nil
}

type messageHandler struct {
	outputChannel chan<- *message.Message
	consumerGroup string
	unmarshaller  Unmarshaller

	nackResendSleep time.Duration

	closing chan struct{}
}

func (h *messageHandler) processMessage(ctx context.Context, stream string, xm *redis.XMessage, messageLogFields watermill.LogFields) error {
	receivedMsgLogFields := messageLogFields.Add(watermill.LogFields{
		"xid": xm.ID,
	})

	log.Effect(ctx, log.LogTrace, "Received message from redis stream", receivedMsgLogFields)

	msg, err := h.unmarshaller.Unmarshal(xm.Values)
	if err != nil {
		return errors.Wrapf(err, "message unmarshal failed")
	}

	ctx, cancelCtx := context.WithCancel(ctx)
	msg.SetContext(ctx)
	defer cancelCtx()

	receivedMsgLogFields = receivedMsgLogFields.Add(watermill.LogFields{
		"message_uuid": msg.UUID,
		"stream":       stream,
		"xid":          xm.ID,
	})

ResendLoop:
	for {
		select {
		case h.outputChannel <- msg:
			log.Effect(ctx, log.LogTrace, "Message sent to consumer", receivedMsgLogFields)
		case <-h.closing:
			log.Effect(ctx, log.LogTrace, "Closing, message discarded", receivedMsgLogFields)
			return nil
		case <-ctx.Done():
			log.Effect(ctx, log.LogTrace, "Closing, ctx cancelled before sent to consumer", receivedMsgLogFields)
			return nil
		}

		select {
		case <-msg.Acked():
			if h.consumerGroup != "" {
				// deadly retry ack
				err := retry.Retry(func(attempt uint) error {
					res := <-dependency.Effect[_redisClient](ctx, XAckEffect{
						ctx:    ctx,
						stream: stream,
						group:  h.consumerGroup,
						ids:    []string{xm.ID},
					})
					return res.Err
				}, func(attempt uint) bool {
					if attempt != 0 {
						time.Sleep(time.Millisecond * 100)
					}
					return true
				}, func(attempt uint) bool {
					select {
					case <-h.closing:
					case <-ctx.Done():
					default:
						return true
					}
					return false
				})
				if err != nil {
					log.Effect(ctx, log.LogError, "Message Acked fail", watermill.LogFields{"error": err}.Add(receivedMsgLogFields))
				}
			}
			log.Effect(ctx, log.LogTrace, "Message Acked", receivedMsgLogFields)
			break ResendLoop
		case <-msg.Nacked():
			log.Effect(ctx, log.LogTrace, "Message Nacked", receivedMsgLogFields)

			// reset acks, etc.
			msg = msg.Copy()
			if h.nackResendSleep != NoSleep {
				time.Sleep(h.nackResendSleep)
			}

			continue ResendLoop
		case <-h.closing:
			log.Effect(ctx, log.LogTrace, "Closing, message discarded before ack", receivedMsgLogFields)
			return nil
		case <-ctx.Done():
			log.Effect(ctx, log.LogTrace, "Closing, ctx cancelled before ack", receivedMsgLogFields)
			return nil
		}
	}

	return nil
}
