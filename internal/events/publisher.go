package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

// publishTimeout bounds one PUBLISH. A slow Redis delays only the publisher's
// own goroutine, never the caller.
const publishTimeout = 2 * time.Second

// bufferSize is how many events can wait to be published. Beyond that,
// new events are dropped: clients recover with a resync.
const bufferSize = 256

// Sender is the one Redis call a Publisher needs. *queue.Queue satisfies it.
type Sender interface {
	Publish(ctx context.Context, channel string, payload []byte) error
}

// Publisher sends job events to Redis in the background. It satisfies
// database.Notifier: stores call it after they commit a change.
//
// Its methods never block and never fail. A job's status is already safe
// in PostgreSQL when they are called; the event is a courtesy to whoever is
// watching, and losing one costs a client a reload, not a job. So a Redis
// outage must not slow down creating a job or recording its result: events
// go into a buffer, one goroutine publishes them in order, and when the
// buffer is full new events are dropped and counted.
type Publisher struct {
	send    Sender
	channel string
	logger  *slog.Logger
	queue   chan []byte
	dropped atomic.Int64
}

// NewPublisher returns a Publisher for Channel. Call Start before use;
// until then, events wait in the buffer.
func NewPublisher(send Sender, logger *slog.Logger) *Publisher {
	return &Publisher{send: send, channel: Channel, logger: logger, queue: make(chan []byte, bufferSize)}
}

// JobChanged publishes a created or changed job to its owner.
func (p *Publisher) JobChanged(_ context.Context, j *job.Job) {
	p.enqueue(j.UserID, Message{Type: TypeChanged, Job: j})
}

// JobDeleted publishes that one of userID's jobs is gone.
func (p *Publisher) JobDeleted(_ context.Context, userID, id string) {
	p.enqueue(userID, Message{Type: TypeDeleted, ID: id})
}

// enqueue encodes now, so later changes to the caller's job can't alter
// the event, and hands it to the background goroutine without waiting.
func (p *Publisher) enqueue(userID string, m Message) {
	b, err := json.Marshal(envelope{UserID: userID, Message: encoded(m)})
	if err != nil {
		panic("events: encode envelope: " + err.Error())
	}
	select {
	case p.queue <- b:
	default:
		p.dropped.Add(1)
	}
}

// Dropped reports how many events were dropped because the buffer was full.
func (p *Publisher) Dropped() int64 { return p.dropped.Load() }

// Start publishes buffered events until ctx ends or stop is called. stop
// waits for the goroutine to finish. Events still in the buffer then are
// dropped: clients resync when they reconnect.
func (p *Publisher) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		failing := false
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-p.queue:
				sendCtx, cancelSend := context.WithTimeout(ctx, publishTimeout)
				err := p.send.Publish(sendCtx, p.channel, b)
				cancelSend()
				// Log the first failure and the recovery, not every event in
				// between: during an outage that would be one line per event.
				switch {
				case err != nil && !failing:
					failing = true
					p.logger.Warn("events: can't publish; live updates are paused, job status is unaffected", "err", err)
				case err == nil && failing:
					failing = false
					p.logger.Info("events: publishing again")
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
