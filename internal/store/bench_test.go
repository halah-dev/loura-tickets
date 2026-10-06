package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/halah-dev/loura-tickets/internal/store"
	"github.com/halah-dev/loura-tickets/internal/ticket"
)

func BenchmarkCreate(b *testing.B) {
	s, _ := store.Open(filepath.Join(b.TempDir(), "b.db"))
	defer s.Close()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Create(ctx, store.NewTicket{ID: fmt.Sprintf("t-%d", i), Subject: "subject", Body: "body"})
	}
}

func BenchmarkClaimAndSave(b *testing.B) {
	s, _ := store.Open(filepath.Join(b.TempDir(), "b.db"))
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		s.Create(ctx, store.NewTicket{ID: fmt.Sprintf("t-%d", i), Subject: "subject", Body: "body"})
	}
	c := ticket.Classification{Category: ticket.CategoryBilling, Priority: ticket.PriorityLow, Summary: "ok"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t, err := s.Claim(ctx, time.Minute)
		if err != nil {
			b.Fatal(err)
		}
		s.SaveResult(ctx, t.ID, c, 1)
	}
}

func BenchmarkGet(b *testing.B) {
	s, _ := store.Open(filepath.Join(b.TempDir(), "b.db"))
	defer s.Close()
	ctx := context.Background()
	s.Create(ctx, store.NewTicket{ID: "t-1", Subject: "subject", Body: "body"})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Get(ctx, "t-1")
	}
}

// BenchmarkGetParallel is the read path under concurrency, which is where the
// connection pool size actually shows up.
func BenchmarkGetParallel(b *testing.B) {
	s, _ := store.Open(filepath.Join(b.TempDir(), "b.db"))
	defer s.Close()
	ctx := context.Background()
	s.Create(ctx, store.NewTicket{ID: "t-1", Subject: "subject", Body: "body"})
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Get(ctx, "t-1")
		}
	})
}

// BenchmarkListParallel is the heavier read: the endpoint a dashboard would hit.
func BenchmarkListParallel(b *testing.B) {
	s, _ := store.Open(filepath.Join(b.TempDir(), "b.db"))
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		s.Create(ctx, store.NewTicket{ID: fmt.Sprintf("t-%d", i), Subject: "subject", Body: "body"})
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.List(ctx, store.ListFilter{Limit: 20})
		}
	})
}

// BenchmarkGetUnderWriteLoad is the real access pattern: API reads served while
// the worker pool is writing. This is the case the connection pool size is
// supposed to affect.
func BenchmarkGetUnderWriteLoad(b *testing.B) {
	s, _ := store.Open(filepath.Join(b.TempDir(), "b.db"))
	defer s.Close()
	ctx := context.Background()
	s.Create(ctx, store.NewTicket{ID: "t-read", Subject: "subject", Body: "body"})

	stop := make(chan struct{})
	defer close(stop)
	for w := 0; w < 4; w++ {
		go func(w int) {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				s.Create(ctx, store.NewTicket{ID: fmt.Sprintf("w%d-%d", w, i), Subject: "subject", Body: "body"})
			}
		}(w)
	}
	time.Sleep(100 * time.Millisecond)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Get(ctx, "t-read")
		}
	})
}
