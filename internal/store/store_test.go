package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/halah-dev/loura-tickets/internal/store"
	"github.com/halah-dev/loura-tickets/internal/ticket"
)

// open gives each test its own database file, so tests do not share state and
// the real file-backed code path is the one under test.
func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateIsIdempotentOnID(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	first, created, err := s.Create(ctx, store.NewTicket{ID: "t-1001", Subject: "Charged twice", Body: "Two charges of 49.00."})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !created {
		t.Fatal("first create reported the ticket already existed")
	}
	if first.Status != ticket.StatusPending {
		t.Errorf("status = %q, want pending", first.Status)
	}

	// A different payload under the same id must not overwrite the original.
	// An upstream mail poller re-delivering is the expected cause, and the
	// first version is the one we already queued for classification.
	second, created, err := s.Create(ctx, store.NewTicket{ID: "t-1001", Subject: "DIFFERENT SUBJECT", Body: "different body"})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("second create reported a new ticket; submitting the same id twice must not create a duplicate")
	}
	if second.Subject != first.Subject || second.Body != first.Body {
		t.Errorf("re-submitting overwrote the ticket: got subject %q body %q", second.Subject, second.Body)
	}

	page, err := s.List(ctx, store.ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Tickets) != 1 {
		t.Errorf("store holds %d tickets, want 1", len(page.Tickets))
	}
}

// TestCreateDoesNotResetAClassifiedTicket: re-ingesting must not send a ticket
// back round the lifecycle, or an upstream retry would re-run classification.
func TestCreateDoesNotResetAClassifiedTicket(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if _, _, err := s.Create(ctx, store.NewTicket{ID: "t-1", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	result := ticket.Classification{Category: ticket.CategoryBilling, Priority: ticket.PriorityLow, Summary: "Done."}
	if err := s.SaveResult(ctx, "t-1", result, 1); err != nil {
		t.Fatal(err)
	}

	got, _, err := s.Create(ctx, store.NewTicket{ID: "t-1", Subject: "s", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusClassified {
		t.Errorf("status = %q after re-ingest, want it to stay classified", got.Status)
	}
	if _, err := s.Claim(ctx, time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Error("a classified ticket became claimable again after re-ingest")
	}
}

// TestClaimIsExclusive is the core concurrency guarantee: with many workers
// racing, every ticket is handed to exactly one of them.
func TestClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	const tickets = 50
	for i := 0; i < tickets; i++ {
		if _, _, err := s.Create(ctx, store.NewTicket{ID: id(i), Subject: "subject", Body: "body"}); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu     sync.Mutex
		claims = map[string]int{}
		wg     sync.WaitGroup
	)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := s.Claim(ctx, time.Minute)
				if errors.Is(err, store.ErrNotFound) {
					return
				}
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				claims[got.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claims) != tickets {
		t.Errorf("claimed %d distinct tickets, want %d", len(claims), tickets)
	}
	for id, n := range claims {
		if n != 1 {
			t.Errorf("ticket %s was claimed %d times, want exactly 1", id, n)
		}
	}
}

func TestClaimSkipsLeasedTicketsUntilTheLeaseExpires(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if _, _, err := s.Create(ctx, store.NewTicket{ID: "t-1", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}

	first, err := s.Claim(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.Attempts != 1 {
		t.Errorf("attempts = %d after one claim, want 1", first.Attempts)
	}

	if _, err := s.Claim(ctx, time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a leased ticket was claimed again before its lease expired")
	}

	// This is the restart story: a worker that dies holding a ticket leaves a
	// lease behind, and the ticket becomes available again when it runs out.
	time.Sleep(80 * time.Millisecond)

	retry, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatalf("claim after lease expiry: %v", err)
	}
	if retry.ID != "t-1" {
		t.Errorf("claimed %q, want t-1", retry.ID)
	}
	if retry.Attempts != 2 {
		t.Errorf("attempts = %d, want the second claim to count as a second attempt", retry.Attempts)
	}
}

// TestTerminalWritesHappenOnce covers the double-processing case: if a lease
// expired while a slow classification was still running, two workers may both
// produce a verdict. Only the first may land.
func TestTerminalWritesHappenOnce(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if _, _, err := s.Create(ctx, store.NewTicket{ID: "t-1", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}

	winner := ticket.Classification{Category: ticket.CategoryBilling, Priority: ticket.PriorityHigh, Summary: "First verdict."}
	loser := ticket.Classification{Category: ticket.CategoryOther, Priority: ticket.PriorityLow, Summary: "Second verdict."}

	if err := s.SaveResult(ctx, "t-1", winner, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveResult(ctx, "t-1", loser, 1); err != nil {
		t.Fatalf("the late write should be a silent no-op, not an error: %v", err)
	}
	if err := s.Fail(ctx, "t-1", "too late"); err != nil {
		t.Fatalf("a late failure should be a silent no-op, not an error: %v", err)
	}

	got, err := s.Get(ctx, "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ticket.StatusClassified {
		t.Errorf("status = %q, want classified: the late Fail must not overwrite a verdict", got.Status)
	}
	if *got.Summary != winner.Summary {
		t.Errorf("summary = %q, want the first verdict %q", *got.Summary, winner.Summary)
	}
}

func TestReleaseMakesATicketClaimableAgainAtNotBefore(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if _, _, err := s.Create(ctx, store.NewTicket{ID: "t-1", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}

	// Released into the future: still not claimable (this is retry backoff).
	if err := s.Release(ctx, "t-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Error("a ticket released with a future notBefore was claimed early, defeating backoff")
	}

	// Released immediately: claimable now (this is the shutdown hand-back).
	if err := s.Release(ctx, "t-1", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, time.Minute); err != nil {
		t.Errorf("a ticket handed back on shutdown was not picked up again: %v", err)
	}
}

func TestGetUnknownIDReturnsNotFound(t *testing.T) {
	if _, err := open(t).Get(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListFiltersAndPages(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	// Six tickets: three billing/high, two technical/low, one left pending.
	seed := []struct {
		id       string
		category ticket.Category
		priority ticket.Priority
	}{
		{"t-1", ticket.CategoryBilling, ticket.PriorityHigh},
		{"t-2", ticket.CategoryBilling, ticket.PriorityHigh},
		{"t-3", ticket.CategoryBilling, ticket.PriorityHigh},
		{"t-4", ticket.CategoryTechnical, ticket.PriorityLow},
		{"t-5", ticket.CategoryTechnical, ticket.PriorityLow},
	}
	for _, sd := range seed {
		if _, _, err := s.Create(ctx, store.NewTicket{ID: sd.id, Subject: "s", Body: "b"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(ctx, time.Minute); err != nil {
			t.Fatal(err)
		}
		c := ticket.Classification{Category: sd.category, Priority: sd.priority, Summary: "ok"}
		if err := s.SaveResult(ctx, sd.id, c, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Create(ctx, store.NewTicket{ID: "t-6", Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}

	billing := ticket.CategoryBilling
	high := ticket.PriorityHigh
	pending := ticket.StatusPending

	t.Run("filter by category", func(t *testing.T) {
		page, err := s.List(ctx, store.ListFilter{Category: &billing, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Tickets) != 3 {
			t.Errorf("got %d tickets, want 3", len(page.Tickets))
		}
	})

	t.Run("filter by category and priority together", func(t *testing.T) {
		page, err := s.List(ctx, store.ListFilter{Category: &billing, Priority: &high, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Tickets) != 3 {
			t.Errorf("got %d tickets, want 3", len(page.Tickets))
		}
	})

	t.Run("filter by status", func(t *testing.T) {
		page, err := s.List(ctx, store.ListFilter{Status: &pending, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Tickets) != 1 || page.Tickets[0].ID != "t-6" {
			t.Errorf("got %+v, want only the pending ticket t-6", page.Tickets)
		}
	})

	t.Run("pagination walks every ticket exactly once", func(t *testing.T) {
		seen := map[string]int{}
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("pagination did not terminate")
			}
			page, err := s.List(ctx, store.ListFilter{Limit: 2, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, tk := range page.Tickets {
				seen[tk.ID]++
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if len(seen) != 6 {
			t.Errorf("saw %d distinct tickets across all pages, want 6", len(seen))
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("ticket %s appeared on %d pages, want 1", id, n)
			}
		}
	})

	t.Run("last page carries no cursor", func(t *testing.T) {
		page, err := s.List(ctx, store.ListFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if page.NextCursor != "" {
			t.Errorf("next_cursor = %q on a complete page, want empty", page.NextCursor)
		}
	})

	t.Run("a forged cursor is rejected", func(t *testing.T) {
		if _, err := s.List(ctx, store.ListFilter{Limit: 10, Cursor: "not-a-cursor"}); !errors.Is(err, store.ErrBadCursor) {
			t.Errorf("err = %v, want ErrBadCursor", err)
		}
	})
}

func id(i int) string {
	return "t-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}
