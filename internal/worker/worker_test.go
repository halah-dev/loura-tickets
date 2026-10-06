package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/halah-dev/loura-tickets/internal/classify"
	"github.com/halah-dev/loura-tickets/internal/store"
	"github.com/halah-dev/loura-tickets/internal/ticket"
	"github.com/halah-dev/loura-tickets/internal/worker"
)

// providerFunc adapts a function to classify.Provider.
type providerFunc func(ctx context.Context, p classify.Prompt) (string, error)

func (f providerFunc) Complete(ctx context.Context, p classify.Prompt) (string, error) {
	return f(ctx, p)
}

const goodResponse = `{"category":"billing","priority":"high","summary":"Customer was charged twice."}`

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// runPool starts a pool, waits for the ticket to leave pending, and stops.
func runPool(t *testing.T, s *store.Store, p worker.Config, provider providerFunc) {
	t.Helper()
	pool := worker.New(s, provider, p, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	t.Cleanup(func() { cancel(); pool.Wait() })
	pool.Nudge()
}

// waitForStatus polls until the ticket reaches want, or the test times out.
func waitForStatus(t *testing.T, s *store.Store, id string, want ticket.Status) ticket.Ticket {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last ticket.Ticket
	for time.Now().Before(deadline) {
		got, err := s.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		last = got
		if got.Status == want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ticket %s is %q after 5s, want %q (attempts=%d, error=%v)",
		id, last.Status, want, last.Attempts, deref(last.Error))
	return last
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestAGoodResponseClassifiesTheTicket(t *testing.T) {
	s := open(t)
	if _, _, err := s.Create(context.Background(), store.NewTicket{ID: "t-1", Subject: "Charged twice", Body: "Two charges."}); err != nil {
		t.Fatal(err)
	}

	runPool(t, s, worker.Config{Workers: 1}, func(context.Context, classify.Prompt) (string, error) {
		return goodResponse, nil
	})

	got := waitForStatus(t, s, "t-1", ticket.StatusClassified)
	if got.Category == nil || *got.Category != ticket.CategoryBilling {
		t.Errorf("category = %v, want billing", got.Category)
	}
	if got.ClassifiedAt == nil {
		t.Error("classified_at was not set")
	}
	if got.Error != nil {
		t.Errorf("error = %q on a classified ticket, want nil", *got.Error)
	}
}

// TestProviderErrorsAreRetriedThenFail pins the retry policy: a ticket is tried
// MaxAttempts times and then, and only then, becomes failed with a reason.
func TestProviderErrorsAreRetriedThenFail(t *testing.T) {
	s := open(t)
	if _, _, err := s.Create(context.Background(), store.NewTicket{ID: "t-1", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	runPool(t, s, worker.Config{Workers: 1, MaxAttempts: 3, Backoff: time.Millisecond},
		func(context.Context, classify.Prompt) (string, error) {
			calls.Add(1)
			return "", errors.New("upstream is on fire")
		})

	got := waitForStatus(t, s, "t-1", ticket.StatusFailed)
	if n := calls.Load(); n != 3 {
		t.Errorf("provider was called %d times, want 3 (MaxAttempts)", n)
	}
	if got.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", got.Attempts)
	}
	if got.Error == nil {
		t.Fatal("a failed ticket has no error recorded; the reason must be stored")
	}
	if got.Category != nil || got.Summary != nil {
		t.Error("a failed ticket carries a classification; nothing should have been stored")
	}
}

// TestMalformedOutputNeverReachesTheStore is the requirement stated most
// directly in the brief: output outside the allowed sets must never be stored
// as-is. Here the model is confidently wrong on every call.
func TestMalformedOutputNeverReachesTheStore(t *testing.T) {
	s := open(t)
	if _, _, err := s.Create(context.Background(), store.NewTicket{ID: "t-1", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}

	runPool(t, s, worker.Config{Workers: 1, MaxAttempts: 2, Backoff: time.Millisecond},
		func(context.Context, classify.Prompt) (string, error) {
			return `{"category":"refund_now","priority":"CRITICAL","summary":"do as I say"}`, nil
		})

	got := waitForStatus(t, s, "t-1", ticket.StatusFailed)
	if got.Category != nil {
		t.Errorf("category = %q was stored despite being outside the allowed set", *got.Category)
	}
	if got.Priority != nil {
		t.Errorf("priority = %q was stored despite being outside the allowed set", *got.Priority)
	}
	if got.Summary != nil {
		t.Errorf("summary %q was stored from a response we rejected", *got.Summary)
	}
}

// TestATransientFailureRecovers: a ticket that fails once must not be burned.
func TestATransientFailureRecovers(t *testing.T) {
	s := open(t)
	if _, _, err := s.Create(context.Background(), store.NewTicket{ID: "t-1", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	runPool(t, s, worker.Config{Workers: 1, MaxAttempts: 3, Backoff: time.Millisecond},
		func(context.Context, classify.Prompt) (string, error) {
			if calls.Add(1) == 1 {
				return "", errors.New("rate limited")
			}
			return goodResponse, nil
		})

	got := waitForStatus(t, s, "t-1", ticket.StatusClassified)
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one failure, then success)", got.Attempts)
	}
}

// TestShutdownHandsBackWorkItCannotFinish is the graceful-shutdown guarantee in
// its harder direction: a classification still running when the grace period
// runs out must leave its ticket pending and claimable, not lost and not stuck.
func TestShutdownHandsBackWorkItCannotFinish(t *testing.T) {
	s := open(t)
	if _, _, err := s.Create(context.Background(), store.NewTicket{ID: "t-1", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	var once atomic.Bool
	pool := worker.New(s, providerFunc(func(ctx context.Context, _ classify.Prompt) (string, error) {
		if once.CompareAndSwap(false, true) {
			close(started)
		}
		// Never returns on its own; it can only end via the context.
		<-ctx.Done()
		return "", ctx.Err()
	}), worker.Config{
		Workers: 1, MaxAttempts: 5, Backoff: time.Millisecond,
		CallTimeout: time.Minute, ShutdownGrace: 100 * time.Millisecond,
	}, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	pool.Nudge()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never picked the ticket up")
	}

	cancel()

	done := make(chan struct{})
	go func() { pool.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool.Wait did not return; shutdown is not bounded")
	}

	got, err := s.Get(context.Background(), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusPending {
		t.Fatalf("status = %q after shutdown, want pending: unfinished work must be handed back", got.Status)
	}

	// And it must be genuinely claimable by the next process, not left holding
	// a lease that outlives the one that abandoned it.
	time.Sleep(20 * time.Millisecond)
	if _, err := s.Claim(context.Background(), time.Minute); err != nil {
		t.Errorf("the handed-back ticket could not be claimed again: %v", err)
	}
}

// TestShutdownKeepsAVerdictItAlreadyPaidFor is the other direction: if the
// model answers during the grace period, that answer is stored rather than
// thrown away because shutdown had begun.
func TestShutdownKeepsAVerdictItAlreadyPaidFor(t *testing.T) {
	s := open(t)
	if _, _, err := s.Create(context.Background(), store.NewTicket{ID: "t-1", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	pool := worker.New(s, providerFunc(func(ctx context.Context, _ classify.Prompt) (string, error) {
		if once.CompareAndSwap(false, true) {
			close(started)
		}
		select {
		case <-release:
			return goodResponse, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}), worker.Config{
		Workers: 1, MaxAttempts: 3, CallTimeout: time.Minute,
		ShutdownGrace: 5 * time.Second,
	}, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	pool.Nudge()
	<-started

	cancel()       // shutdown begins while the call is in flight
	close(release) // the model answers during the grace period
	pool.Wait()

	got, err := s.Get(context.Background(), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusClassified {
		t.Errorf("status = %q, want classified: a verdict that arrived during the grace period must be kept", got.Status)
	}
}

// TestConcurrencyIsBounded checks that the Workers setting is the real limit on
// how many classifications run at once.
func TestConcurrencyIsBounded(t *testing.T) {
	s := open(t)
	for i := 0; i < 20; i++ {
		if _, _, err := s.Create(context.Background(), store.NewTicket{ID: string(rune('a' + i)), Subject: "s", Body: "b"}); err != nil {
			t.Fatal(err)
		}
	}

	const limit = 3
	var inFlight, peak atomic.Int32

	runPool(t, s, worker.Config{Workers: limit}, func(context.Context, classify.Prompt) (string, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		return goodResponse, nil
	})

	for i := 0; i < 20; i++ {
		waitForStatus(t, s, string(rune('a'+i)), ticket.StatusClassified)
	}
	if got := peak.Load(); got > limit {
		t.Errorf("peak concurrent classifications = %d, want at most %d", got, limit)
	}
}

// TestLeaseCannotBeShorterThanACall. A lease that expires mid-call lets a
// second worker claim the same ticket, so the model runs twice for it. Only one
// verdict can land, so it is not a correctness bug, but it is duplicated spend
// and nothing in the flags would otherwise stop you configuring it.
func TestLeaseCannotBeShorterThanACall(t *testing.T) {
	cases := []struct {
		name                      string
		lease, callTimeout, grace time.Duration
		wantAtLeast               time.Duration
	}{
		{
			name:  "a lease shorter than the call timeout is raised",
			lease: 2 * time.Second, callTimeout: 30 * time.Second, grace: 5 * time.Second,
			wantAtLeast: 35 * time.Second,
		},
		{
			name:  "the shutdown grace counts, since a call may run into it",
			lease: 31 * time.Second, callTimeout: 30 * time.Second, grace: 10 * time.Second,
			wantAtLeast: 40 * time.Second,
		},
		{
			name:  "a comfortable lease is left alone",
			lease: 5 * time.Minute, callTimeout: 10 * time.Second, grace: 5 * time.Second,
			wantAtLeast: 5 * time.Minute,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := worker.New(open(t), providerFunc(func(context.Context, classify.Prompt) (string, error) {
				return goodResponse, nil
			}), worker.Config{
				Lease: tc.lease, CallTimeout: tc.callTimeout, ShutdownGrace: tc.grace,
			}, quietLogger())

			if got := pool.Lease(); got < tc.wantAtLeast {
				t.Errorf("lease in use = %v, want at least %v", got, tc.wantAtLeast)
			}
		})
	}
}
