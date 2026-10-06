// Package httpapi exposes the service over HTTP.
//
//	POST /tickets          ingest (idempotent on id)
//	GET  /tickets/{id}     one ticket
//	GET  /tickets          list, filterable and paged
//	GET  /healthz          liveness
//
// Errors share one shape so a caller can branch on code without parsing prose.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/halah-dev/loura-tickets/internal/classify"
	"github.com/halah-dev/loura-tickets/internal/store"
	"github.com/halah-dev/loura-tickets/internal/ticket"
)

const (
	// maxBodyBytes bounds an ingest request. Ticket bodies are email-sized.
	maxBodyBytes = 64 << 10
	// maxIDLen and maxSubjectLen keep obviously abusive input out of the store.
	maxIDLen      = 128
	maxSubjectLen = 998 // RFC 5322 line limit; subjects arrive from email.

	defaultLimit = 20
	maxLimit     = 100
)

// Notifier is told when a ticket has been ingested, so classification can start
// without waiting for the next poll. The worker pool satisfies it.
type Notifier interface{ Nudge() }

// Store is the part of the ticket store the handlers use, declared here for the
// same reason as worker.Store. ListFilter and Page stay in the store package:
// they are data, and both sides share them.
type Store interface {
	Create(ctx context.Context, nt store.NewTicket) (ticket.Ticket, bool, error)
	Get(ctx context.Context, id string) (ticket.Ticket, error)
	List(ctx context.Context, f store.ListFilter) (store.Page, error)
}

// Server wires the store and the worker pool to an HTTP router.
type Server struct {
	store    Store
	notifier Notifier
	log      *slog.Logger
}

// NewServer returns a Server. notifier may be nil.
func NewServer(s Store, n Notifier, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: s, notifier: n, log: log}
}

// Routes returns the handler for the service.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tickets", s.handleIngest)
	mux.HandleFunc("GET /tickets", s.handleList)
	mux.HandleFunc("GET /tickets/{id}", s.handleGet)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return s.withRequestID(mux)
}

// ingestRequest is the ingest payload. Subject may be empty: t-1008 has none,
// and rejecting it would drop a real ticket. Body may not be.
type ingestRequest struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req ingestRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	req.ID = strings.TrimSpace(req.ID)
	switch {
	case req.ID == "":
		writeError(w, r, http.StatusBadRequest, "invalid_request", "id is required")
		return
	case len(req.ID) > maxIDLen:
		writeError(w, r, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("id must be at most %d bytes", maxIDLen))
		return
	case len(req.Subject) > maxSubjectLen:
		writeError(w, r, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("subject must be at most %d bytes", maxSubjectLen))
		return
	case strings.TrimSpace(req.Body) == "":
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body is required")
		return
	}

	// Runs before any model call: it reads only the ticket text. A flagged ticket
	// is still classified, the flag just means someone should look.
	flagged, reasons := classify.Suspicious(req.Subject, req.Body)

	t, created, err := s.store.Create(r.Context(), store.NewTicket{
		ID: req.ID, Subject: req.Subject, Body: req.Body,
		ReviewRequired: flagged, ReviewReasons: reasons,
	})
	if err != nil {
		s.internal(w, r, "ingesting ticket", err)
		return
	}
	if created && flagged {
		// Reasons to the log, not the response: naming the rule is an evasion guide.
		s.logger(r.Context()).Warn("ticket flagged for review",
			"ticket", t.ID, "reasons", strings.Join(reasons, ","))
	}

	// 201 new, 200 already known. A mail poller retrying is expected, not an
	// error, but the caller should still be able to tell which happened.
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if s.notifier != nil {
			s.notifier.Nudge()
		}
	}
	writeJSON(w, status, newTicketResponse(t))
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, "not_found", "no ticket with that id")
		return
	}
	if err != nil {
		s.internal(w, r, "fetching ticket", err)
		return
	}
	writeJSON(w, http.StatusOK, newTicketResponse(t))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ListFilter{Limit: defaultLimit, Cursor: q.Get("cursor")}

	// An unknown filter value is a 400, not an empty page: a typo should be loud.
	if v := q.Get("category"); v != "" {
		c, err := ticket.ParseCategory(v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", err.Error())
			return
		}
		f.Category = &c
	}
	if v := q.Get("priority"); v != "" {
		p, err := ticket.ParsePriority(v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", err.Error())
			return
		}
		f.Priority = &p
	}
	if v := q.Get("status"); v != "" {
		st := ticket.Status(v)
		if st != ticket.StatusPending && st != ticket.StatusClassified && st != ticket.StatusFailed {
			writeError(w, r, http.StatusBadRequest, "invalid_filter",
				fmt.Sprintf("status %q is not one of [pending classified failed]", v))
			return
		}
		f.Status = &st
	}
	if v := q.Get("review_required"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_filter",
				"review_required must be true or false")
			return
		}
		f.ReviewRequired = &b
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", "limit must be a positive integer")
			return
		}
		f.Limit = min(n, maxLimit)
	}

	page, err := s.store.List(r.Context(), f)
	if errors.Is(err, store.ErrBadCursor) {
		writeError(w, r, http.StatusBadRequest, "invalid_cursor", "cursor is not one we issued")
		return
	}
	if err != nil {
		s.internal(w, r, "listing tickets", err)
		return
	}

	out := make([]ticketResponse, 0, len(page.Tickets))
	for _, t := range page.Tickets {
		out = append(out, newTicketResponse(t))
	}
	writeJSON(w, http.StatusOK, listResponse{Tickets: out, NextCursor: page.NextCursor})
}

// internal logs the real error and tells the caller nothing but the request id.
func (s *Server) internal(w http.ResponseWriter, r *http.Request, doing string, err error) {
	s.logger(r.Context()).Error(doing+" failed", "error", err)
	writeError(w, r, http.StatusInternalServerError, "internal", "something went wrong on our side")
}

type listResponse struct {
	Tickets []ticketResponse `json:"tickets"`
	// NextCursor is absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

type classificationResponse struct {
	Category string `json:"category"`
	Priority string `json:"priority"`
	Summary  string `json:"summary"`
}

// ticketResponse omits the attempt count: it describes the retry policy, not the
// ticket, and exposing it would make -max-attempts a breaking change. It stays
// in the store and the logs.
type ticketResponse struct {
	ID             string                  `json:"id"`
	Subject        string                  `json:"subject"`
	Body           string                  `json:"body"`
	Status         string                  `json:"status"`
	Classification *classificationResponse `json:"classification"`
	// Always present: a caller judging whether to trust a classification should
	// not have to know an absent field means no.
	ReviewRequired bool       `json:"review_required"`
	Error          *string    `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ClassifiedAt   *time.Time `json:"classified_at,omitempty"`
}

func newTicketResponse(t ticket.Ticket) ticketResponse {
	r := ticketResponse{
		ID: t.ID, Subject: t.Subject, Body: t.Body,
		Status: string(t.Status), Error: t.Error, ReviewRequired: t.ReviewRequired,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, ClassifiedAt: t.ClassifiedAt,
	}
	// Written together or not at all, so no caller sees a half-classified ticket.
	if t.Status == ticket.StatusClassified && t.Category != nil && t.Priority != nil && t.Summary != nil {
		r.Classification = &classificationResponse{
			Category: string(*t.Category), Priority: string(*t.Priority), Summary: *t.Summary,
		}
	}
	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writing response failed", "error", err)
	}
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// RequestID lets a caller quote one string back to us instead of a
	// timestamp and a description of what they were doing.
	RequestID string `json:"request_id,omitempty"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, map[string]errorBody{"error": {
		Code: code, Message: message, RequestID: RequestIDFrom(r.Context()),
	}})
}
