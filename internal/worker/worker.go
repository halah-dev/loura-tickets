// Package worker runs classification outside the HTTP request.
//
// A fixed pool of goroutines claims one ticket at a time from the store. There
// is no in-memory queue: the queue is the tickets table, which is what makes a
// restart safe. Anything in flight when the process dies is a leased row whose
// lease runs out.
//
// On shutdown workers stop claiming. One holding a ticket gets a bounded grace
// period to record its verdict, and releases the ticket if it cannot.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/halah-dev/loura-tickets/internal/classify"
	"github.com/halah-dev/loura-tickets/internal/store"
	"github.com/halah-dev/loura-tickets/internal/ticket"
)

// Store is the part of the ticket store the pool uses, declared at the consumer
// so the dependency is visible from here. Swapping SQLite means satisfying this.
type Store interface {
	Claim(ctx context.Context, lease time.Duration) (ticket.Ticket, error)
	SaveResult(ctx context.Context, id string, c ticket.Classification, promptVersion int) error
	Fail(ctx context.Context, id, reason string) error
	Release(ctx context.Context, id string, notBefore time.Time) error
}

// Config tunes the pool. Zero values are replaced with the defaults below.
type Config struct {
	// Workers is the maximum number of classifications running at once.
	Workers int
	// MaxAttempts is how many times a ticket may be tried before it fails.
	MaxAttempts int
	// Backoff is the delay before the first retry; it doubles each attempt.
	Backoff time.Duration
	// Lease is how long a claim is held before another worker may take over.
	// It must comfortably exceed CallTimeout.
	Lease time.Duration
	// CallTimeout bounds a single provider call.
	CallTimeout time.Duration
	// PollInterval is how often an idle worker looks for work. Ingest also
	// nudges the pool directly, so this is only a backstop.
	PollInterval time.Duration
	// ShutdownGrace is how long an in-flight classification may continue after
	// shutdown begins before its ticket is handed back.
	ShutdownGrace time.Duration
}

func (c Config) withDefaults() Config {
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.Backoff <= 0 {
		c.Backoff = 500 * time.Millisecond
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 10 * time.Second
	}
	if c.Lease <= 0 {
		c.Lease = 2 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 5 * time.Second
	}
	return c
}

// Pool classifies pending tickets.
type Pool struct {
	store    Store
	provider classify.Provider
	cfg      Config
	log      *slog.Logger

	wake chan struct{}
	wg   sync.WaitGroup
}

// New builds a pool. Call Start to run it.
func New(s Store, p classify.Provider, cfg Config, log *slog.Logger) *Pool {
	if log == nil {
		log = slog.Default()
	}

	cfg = cfg.withDefaults()

	// A lease has to outlast the work it protects. If it expires mid-call another
	// worker claims the same ticket and the model runs twice for it. Only one
	// verdict lands, so this is wasted spend rather than a correctness bug, but
	// nothing in the flags would otherwise stop you configuring it.
	if floor := cfg.CallTimeout + cfg.ShutdownGrace + time.Second; cfg.Lease < floor {
		log.Warn("lease is shorter than a call can run, raising it",
			"configured", cfg.Lease, "using", floor,
			"call_timeout", cfg.CallTimeout, "shutdown_grace", cfg.ShutdownGrace)
		cfg.Lease = floor
	}

	return &Pool{
		store:    s,
		provider: p,
		cfg:      cfg,
		log:      log,
		wake:     make(chan struct{}, 1),
	}
}

// Lease reports the lease duration actually in use, which may be longer than
// the one configured. Exposed so the invariant above is testable.
func (p *Pool) Lease() time.Duration { return p.cfg.Lease }

// Nudge wakes an idle worker. Never blocks: a missed nudge costs one poll
// interval, not a stuck ticket.
func (p *Pool) Nudge() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Start launches the workers. They stop when ctx is cancelled.
func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.cfg.Workers; i++ {
		p.wg.Add(1)
		go func(n int) {
			defer p.wg.Done()
			p.run(ctx, p.log.With("worker", n))
		}(i)
	}
}

// Wait blocks until every worker has stopped and handed back or finished its
// ticket. Call it after cancelling the context passed to Start.
func (p *Pool) Wait() { p.wg.Wait() }

func (p *Pool) run(ctx context.Context, log *slog.Logger) {
	timer := time.NewTimer(p.cfg.PollInterval)
	defer timer.Stop()

	for {
		// Drain before sleeping, so a burst of ingests is not paced by the poll.
		for {
			if ctx.Err() != nil {
				return
			}
			t, err := p.store.Claim(ctx, p.cfg.Lease)
			if errors.Is(err, store.ErrNotFound) {
				break
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error("claim failed", "error", err)
				break
			}
			p.process(ctx, t, log.With("ticket", t.ID, "attempt", t.Attempts))
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(p.cfg.PollInterval)

		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		}
	}
}

// process runs one classification attempt and records its outcome.
func (p *Pool) process(ctx context.Context, t ticket.Ticket, log *slog.Logger) {
	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	raw, err := p.provider.Complete(callCtx, classify.BuildPrompt(t))
	if err != nil {
		p.retryOrFail(ctx, t, "provider call failed: "+err.Error(), log)
		return
	}

	result, err := classify.Validate(raw)
	if err != nil {
		// Answered, but not with something we will store. The next sample may be
		// fine, so this retries.
		p.retryOrFail(ctx, t, err.Error(), log)
		return
	}

	// Outlives shutdown: a result we paid for should not be dropped at the last
	// step.
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelWrite()

	if err := p.store.SaveResult(writeCtx, t.ID, result, classify.PromptVersion); err != nil {
		log.Error("storing classification failed", "error", err)
		p.release(t.ID, time.Now())
		return
	}
	log.Info("classified", "category", result.Category, "priority", result.Priority)
}

// callContext builds the context for one provider call. It is detached from the
// pool's context so Ctrl-C does not abandon a call we have already paid for,
// then cancelled by whichever comes first: CallTimeout, or ShutdownGrace after
// shutdown begins. Without the second, shutdown would not be bounded.
func (p *Pool) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.CallTimeout)

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Shutdown started. Give the call the grace period, then cut it.
			grace := time.NewTimer(p.cfg.ShutdownGrace)
			defer grace.Stop()
			select {
			case <-grace.C:
				cancel()
			case <-done:
			}
		case <-done:
		}
	}()

	return callCtx, func() {
		close(done)
		cancel()
	}
}

// retryOrFail either hands the ticket back for another attempt after a backoff,
// or marks it failed once the attempt budget is spent.
func (p *Pool) retryOrFail(ctx context.Context, t ticket.Ticket, reason string, log *slog.Logger) {
	if t.Attempts >= p.cfg.MaxAttempts {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := p.store.Fail(writeCtx, t.ID, reason); err != nil {
			log.Error("marking ticket failed did not work", "error", err)
			return
		}
		log.Warn("ticket failed", "reason", reason, "attempts", t.Attempts)
		return
	}

	backoff := p.cfg.Backoff << (t.Attempts - 1)
	log.Warn("classification attempt failed, will retry", "reason", reason, "backoff", backoff)
	p.release(t.ID, time.Now().Add(backoff))
}

func (p *Pool) release(id string, notBefore time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.store.Release(ctx, id, notBefore); err != nil {
		p.log.Error("releasing ticket failed", "ticket", id, "error", err)
	}
}
