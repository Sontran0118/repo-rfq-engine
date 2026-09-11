// Package api exposes the RFQ lifecycle over HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Sontran0118/repo-rfq-engine/internal/domain"
	"github.com/Sontran0118/repo-rfq-engine/internal/store"
)

// Server wires the store to HTTP handlers and a live event stream.
type Server struct {
	store  *store.Store
	events *Broker
	log    *slog.Logger
}

// New builds a Server. The caller owns the store's lifetime.
func New(s *store.Store, log *slog.Logger) *Server {
	return &Server{store: s, events: NewBroker(), log: log}
}

// Routes returns the HTTP handler for the whole API.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/events", s.events.Handle)

	mux.HandleFunc("GET /api/counterparties", s.listCounterparties)
	mux.HandleFunc("POST /api/counterparties", s.createCounterparty)

	mux.HandleFunc("GET /api/rfqs", s.listRFQs)
	mux.HandleFunc("POST /api/rfqs", s.createRFQ)
	mux.HandleFunc("GET /api/rfqs/{id}", s.getRFQ)
	mux.HandleFunc("POST /api/rfqs/{id}/quotes", s.submitQuote)
	mux.HandleFunc("POST /api/rfqs/{id}/accept", s.acceptQuote)
	mux.HandleFunc("POST /api/rfqs/{id}/cancel", s.cancelRFQ)

	mux.HandleFunc("GET /api/trades", s.listTrades)

	return withCORS(withLogging(s.log, mux))
}

// StartExpiryWorker sweeps expired RFQs on an interval until ctx is cancelled.
// Expiry is enforced on the write path too; this exists so the blotter reflects
// reality without anyone touching the RFQ.
func (s *Server) StartExpiryWorker(ctx context.Context, every time.Duration) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := s.store.ExpireStale(ctx)
				if err != nil {
					s.log.Error("expiry sweep failed", "error", err)
					continue
				}
				if n > 0 {
					s.log.Info("expired stale rfqs", "count", n)
					s.events.Publish(Event{Type: "rfq.expired", Payload: map[string]int64{"count": n}})
				}
			}
		}
	}()
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listCounterparties(w http.ResponseWriter, r *http.Request) {
	cps, err := s.store.ListCounterparties(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cps)
}

func (s *Server) createCounterparty(w http.ResponseWriter, r *http.Request) {
	var c store.Counterparty
	if !decode(w, r, &c) {
		return
	}
	if c.Kind != "CLIENT" && c.Kind != "DEALER" {
		writeErr(w, http.StatusBadRequest, "kind must be CLIENT or DEALER")
		return
	}
	if err := s.store.UpsertCounterparty(r.Context(), c); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// createRFQRequest is the wire shape for raising an RFQ. It is deliberately
// separate from domain.RFQ so that server-controlled fields — state, id,
// timestamps — cannot be set by a client.
type createRFQRequest struct {
	ClientID   string            `json:"clientId"`
	Direction  domain.Direction  `json:"direction"`
	Collateral domain.Collateral `json:"collateral"`
	Notional   domain.Money      `json:"notional"`
	Currency   string            `json:"currency"`
	TermDays   int               `json:"termDays"`
	TTLSeconds int               `json:"ttlSeconds"`
}

func (s *Server) createRFQ(w http.ResponseWriter, r *http.Request) {
	var req createRFQRequest
	if !decode(w, r, &req) {
		return
	}

	if req.TermDays < 1 {
		req.TermDays = 1 // overnight, the most common repo tenor
	}
	if req.TTLSeconds < 1 {
		req.TTLSeconds = 300
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}

	now := time.Now().UTC()
	start := now.Truncate(24 * time.Hour)

	rfq := &domain.RFQ{
		ClientID:   req.ClientID,
		Direction:  req.Direction,
		Collateral: req.Collateral,
		Notional:   req.Notional,
		Currency:   req.Currency,
		StartDate:  start,
		EndDate:    start.AddDate(0, 0, req.TermDays),
		State:      domain.StateOpen,
		ExpiresAt:  now.Add(time.Duration(req.TTLSeconds) * time.Second),
		CreatedAt:  now,
	}

	if err := s.store.CreateRFQ(r.Context(), rfq); err != nil {
		s.fail(w, r, err)
		return
	}

	s.events.Publish(Event{Type: "rfq.created", Payload: rfq})
	writeJSON(w, http.StatusCreated, rfq)
}

func (s *Server) getRFQ(w http.ResponseWriter, r *http.Request) {
	rfq, err := s.store.GetRFQ(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rfq)
}

func (s *Server) listRFQs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rfqs, err := s.store.ListRFQs(r.Context(), r.URL.Query().Get("state"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rfqs)
}

type submitQuoteRequest struct {
	DealerID string             `json:"dealerId"`
	Rate     domain.BasisPoints `json:"rate"`
	Haircut  domain.BasisPoints `json:"haircut"`
}

func (s *Server) submitQuote(w http.ResponseWriter, r *http.Request) {
	var req submitQuoteRequest
	if !decode(w, r, &req) {
		return
	}

	q := &domain.Quote{
		RFQID:    r.PathValue("id"),
		DealerID: req.DealerID,
		Rate:     req.Rate,
		Haircut:  req.Haircut,
	}
	if err := s.store.SubmitQuote(r.Context(), q); err != nil {
		s.fail(w, r, err)
		return
	}

	s.events.Publish(Event{Type: "quote.submitted", Payload: q})
	writeJSON(w, http.StatusCreated, q)
}

type acceptRequest struct {
	// QuoteID is optional. Empty means "accept the best price", which is what
	// the blotter's one-click accept sends.
	QuoteID string `json:"quoteId"`
}

func (s *Server) acceptQuote(w http.ResponseWriter, r *http.Request) {
	var req acceptRequest
	if r.ContentLength > 0 && !decode(w, r, &req) {
		return
	}

	trade, err := s.store.AcceptQuote(r.Context(), r.PathValue("id"), req.QuoteID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.events.Publish(Event{Type: "trade.executed", Payload: trade})
	writeJSON(w, http.StatusCreated, trade)
}

func (s *Server) cancelRFQ(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.CancelRFQ(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}

	s.events.Publish(Event{Type: "rfq.cancelled", Payload: map[string]string{"id": id}})
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "state": "CANCELLED"})
}

func (s *Server) listTrades(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	trades, err := s.store.ListTrades(r.Context(), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, trades)
}

// fail maps a domain or store error onto an HTTP status.
//
// The mapping matters for the UI: a 409 tells the blotter "someone beat you to
// it, refresh", while a 500 would tell it "retry", which is exactly wrong for a
// trade that already booked.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrAlreadyTraded), errors.Is(err, store.ErrNotQuotable):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrIllegalTransition):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrNegativeAmount), errors.Is(err, domain.ErrInvalidTenor):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		if errors.Is(err, domain.ErrValidation) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.log.Error("request failed", "path", r.URL.Path, "error", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// The event stream is long-lived; logging its duration on close is noise.
		if r.URL.Path != "/api/events" {
			log.Info("http",
				"method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration", time.Since(start))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Flush forwards to the underlying writer so SSE streaming survives the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
