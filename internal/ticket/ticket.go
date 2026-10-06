// Package ticket holds the domain types. Knows nothing about HTTP, SQL or the
// LLM: everything depends on this, it depends on nothing.
package ticket

import (
	"fmt"
	"time"
)

// Status is the ticket lifecycle. A ticket is created pending and moves
// exactly once to classified or failed.
type Status string

const (
	StatusPending    Status = "pending"
	StatusClassified Status = "classified"
	StatusFailed     Status = "failed"
)

// Category is the closed set of categories the classifier may return.
type Category string

const (
	CategoryBilling   Category = "billing"
	CategoryTechnical Category = "technical"
	CategoryAccount   Category = "account"
	CategoryOther     Category = "other"
)

// Priority is the closed set of priorities the classifier may return.
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
)

// The allowed sets live in one place so the validator, the prompt and the HTTP
// filter parser cannot drift apart.
var (
	Categories = []Category{CategoryBilling, CategoryTechnical, CategoryAccount, CategoryOther}
	Priorities = []Priority{PriorityLow, PriorityMedium, PriorityHigh}
)

// ParseCategory accepts only an exact member of the allowed set.
func ParseCategory(s string) (Category, error) {
	for _, c := range Categories {
		if string(c) == s {
			return c, nil
		}
	}
	return "", fmt.Errorf("category %q is not one of %v", s, Categories)
}

// ParsePriority accepts only an exact member of the allowed set.
func ParsePriority(s string) (Priority, error) {
	for _, p := range Priorities {
		if string(p) == s {
			return p, nil
		}
	}
	return "", fmt.Errorf("priority %q is not one of %v", s, Priorities)
}

// Classification can only be built by classify.Validate, so holding one is proof
// the model's output was inside the allowed sets.
type Classification struct {
	Category Category
	Priority Priority
	Summary  string
}

// Ticket is a support ticket and, once classified, its classification.
type Ticket struct {
	ID      string
	Subject string
	Body    string
	Status  Status

	// Set only when Status is classified.
	Category *Category
	Priority *Priority
	Summary  *string

	// Attempts counts classification runs started for this ticket, including
	// the one in flight. Error holds the reason the ticket ended up failed.
	Attempts int
	Error    *string

	// ReviewRequired marks content that addresses the classifier rather than
	// describing a problem. Set at ingest from the text alone, so it is known
	// before the model runs. ReviewReasons is for operators, not callers.
	ReviewRequired bool
	ReviewReasons  []string

	CreatedAt    time.Time
	UpdatedAt    time.Time
	ClassifiedAt *time.Time
}
