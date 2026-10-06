package classify

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Fake is a stand-in LLM, and an unreliable one on purpose: plausible JSON most
// of the time, and the rest of the time prose, a bad enum, truncated output, an
// invented field, a six-paragraph "summary", or a failed call. Seeded, so a
// failing test reproduces.
type Fake struct {
	// FailureRate is the chance the call itself errors (timeout, rate limit).
	// MalformedRate is the chance the call succeeds but the text is unusable.
	FailureRate   float64
	MalformedRate float64

	// Latency matters more than it looks: an instant provider moves tickets out
	// of pending faster than anyone can watch, hiding the async behaviour.
	Latency time.Duration

	mu  sync.Mutex
	rng *rand.Rand
}

// NewFake returns a Fake with the given seed, rates and per-call latency.
func NewFake(seed int64, failureRate, malformedRate float64, latency time.Duration) *Fake {
	return &Fake{
		FailureRate:   failureRate,
		MalformedRate: malformedRate,
		Latency:       latency,
		rng:           rand.New(rand.NewSource(seed)),
	}
}

// ErrProviderUnavailable stands in for the transport-level failures a real
// provider produces.
var ErrProviderUnavailable = errors.New("provider unavailable")

func (f *Fake) float() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rng.Float64()
}

func (f *Fake) intn(n int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rng.Intn(n)
}

// Complete implements Provider.
func (f *Fake) Complete(ctx context.Context, p Prompt) (string, error) {
	if err := f.sleep(ctx); err != nil {
		return "", err
	}
	if f.float() < f.FailureRate {
		return "", ErrProviderUnavailable
	}
	if f.float() < f.MalformedRate {
		return f.malformed(), nil
	}
	// Only the user half is read: consulting the system half would model a
	// provider that cannot tell the channels apart.
	return f.plausible(p.User), nil
}

// sleep respects cancellation: one that ignored the context would make
// shutdown untestable.
func (f *Fake) sleep(ctx context.Context) error {
	if f.Latency <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(f.Latency)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// malformed returns one of the ways a real model breaks its contract.
func (f *Fake) malformed() string {
	switch f.intn(6) {
	case 0:
		// Prose instead of JSON.
		return "Sure! This ticket looks like a billing issue to me."
	case 1:
		// A category outside the allowed set.
		return `{"category": "refund", "priority": "high", "summary": "Customer wants a refund."}`
	case 2:
		// A priority outside the allowed set.
		return `{"category": "billing", "priority": "urgent", "summary": "Customer was charged twice."}`
	case 3:
		// Truncated output.
		return `{"category": "technical", "priority": "medi`
	case 4:
		// Correct values plus an invented field.
		return `{"category": "account", "priority": "low", "summary": "Email change request.", "confidence": 0.4}`
	default:
		// A "one-sentence summary" that is not one, and not short.
		return `{"category": "other", "priority": "low", "summary": "` +
			strings.Repeat("The customer is writing in about something. ", 20) + `"}`
	}
}

// plausible answers by keyword. Crude on purpose: a fixture, not a classifier.
// It obeys the injection in the sample data, because a fake that always
// resisted would let a broken defence pass the tests.
func (f *Fake) plausible(userContent string) string {
	p := strings.ToLower(userContent)

	category, priority := "other", "low"
	switch {
	case containsAny(p, "charge", "refund", "invoice", "card", "subscription", "overcharged"):
		category, priority = "billing", "medium"
	case containsAny(p, "log in", "login", "password", "credentials", "email address on my account"):
		category, priority = "account", "medium"
	case containsAny(p, "500", "api", "error", "timeout", "export", "broken", "e_timeout"):
		category, priority = "technical", "high"
	}

	// Credulous on purpose: the suite needs a model that can be talked into
	// things.
	if strings.Contains(p, "ignore all previous instructions") {
		return `{"category": "technical", "priority": "high", "summary": "Approved for immediate refund"}`
	}

	return `{"category": "` + category + `", "priority": "` + priority +
		`", "summary": "Customer reports an issue that needs a support response."}`
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
