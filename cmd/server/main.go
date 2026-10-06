// Command server runs the ticket classification service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/halah-dev/loura-tickets/internal/classify"
	"github.com/halah-dev/loura-tickets/internal/httpapi"
	"github.com/halah-dev/loura-tickets/internal/store"
	"github.com/halah-dev/loura-tickets/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr          = flag.String("addr", ":8080", "address to listen on")
		dbPath        = flag.String("db", "tickets.db", "path to the SQLite database file")
		seedPath      = flag.String("seed", "testdata/tickets.json", "ingest this ticket file at startup; empty to skip")
		workers       = flag.Int("workers", 4, "maximum classifications running at once")
		maxAttempts   = flag.Int("max-attempts", 3, "classification attempts before a ticket is marked failed")
		failureRate   = flag.Float64("llm-failure-rate", 0.15, "fake LLM: chance a call errors outright")
		malformedRate = flag.Float64("llm-malformed-rate", 0.25, "fake LLM: chance a call returns unusable text")
		llmSeed       = flag.Int64("llm-seed", 1, "fake LLM: RNG seed, so runs are reproducible")
		llmLatency    = flag.Duration("llm-latency", 300*time.Millisecond, "fake LLM: how long a call takes")
		shutdownWait  = flag.Duration("shutdown-grace", 5*time.Second, "time allowed for in-flight work to finish on shutdown")
		lease         = flag.Duration("lease", 2*time.Minute, "how long a claimed ticket is held before another worker may retry it")
		callTimeout   = flag.Duration("call-timeout", 10*time.Second, "timeout for a single model call")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// The provider is the only thing that would change to talk to a real model:
	// anything with a Complete(ctx, prompt) (string, error) drops in here, and
	// nothing downstream trusts it any more or less.
	provider := classify.NewFake(*llmSeed, *failureRate, *malformedRate, *llmLatency)

	pool := worker.New(st, provider, worker.Config{
		Workers:       *workers,
		MaxAttempts:   *maxAttempts,
		Lease:         *lease,
		CallTimeout:   *callTimeout,
		ShutdownGrace: *shutdownWait,
	}, log)

	// Signals cancel workerCtx, which is what tells the pool to wind down.
	workerCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	pool.Start(workerCtx)

	if *seedPath != "" {
		n, err := seed(context.Background(), st, *seedPath)
		if err != nil {
			return fmt.Errorf("seeding from %s: %w", *seedPath, err)
		}
		log.Info("seeded sample tickets", "new", n, "file", *seedPath)
		pool.Nudge()
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           httpapi.NewServer(st, pool, log).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "workers", *workers)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		return err
	case <-workerCtx.Done():
	}

	// Shutdown order matters. Stop taking new requests first, so no new ticket
	// is ingested while we are draining; then let the workers finish or hand
	// back what they are holding.
	log.Info("shutting down", "grace", *shutdownWait)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownWait)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown did not complete cleanly", "error", err)
	}

	pool.Wait()
	log.Info("stopped")
	return nil
}

// seedTicket matches the shape of testdata/tickets.json.
type seedTicket struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// seed ingests a ticket file. It goes through the same idempotent Create as the
// HTTP endpoint, so running it on every start is safe and re-seeding never
// re-runs classification on a ticket that already exists.
func seed(ctx context.Context, st *store.Store, path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var tickets []seedTicket
	if err := json.Unmarshal(raw, &tickets); err != nil {
		return 0, err
	}

	var created int
	for _, t := range tickets {
		flagged, reasons := classify.Suspicious(t.Subject, t.Body)
		_, isNew, err := st.Create(ctx, store.NewTicket{
			ID: t.ID, Subject: t.Subject, Body: t.Body,
			ReviewRequired: flagged, ReviewReasons: reasons,
		})
		if err != nil {
			return created, fmt.Errorf("ticket %s: %w", t.ID, err)
		}
		if isNew {
			created++
			if flagged {
				slog.Warn("ticket flagged for review",
					"ticket", t.ID, "reasons", strings.Join(reasons, ","))
			}
		}
	}
	return created, nil
}
