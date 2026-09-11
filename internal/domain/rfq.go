package domain

import (
	"errors"
	"fmt"
	"time"
)

// Direction is the side the requesting client takes.
//
// In a repo the client borrows cash and delivers securities as collateral, so
// the client pays the repo rate and wants it as low as possible. In a reverse
// repo the client lends cash against securities, earns the rate, and wants it
// as high as possible. This single flag inverts what "best quote" means, which
// is why it is modelled explicitly rather than inferred at the call site.
type Direction string

const (
	// Repo: client borrows cash, delivers collateral, pays the rate.
	Repo Direction = "REPO"
	// ReverseRepo: client lends cash, receives collateral, earns the rate.
	ReverseRepo Direction = "REVERSE_REPO"
)

func (d Direction) Valid() bool { return d == Repo || d == ReverseRepo }

// RFQState is the lifecycle position of a request for quote.
type RFQState string

const (
	// StateOpen: live and accepting quotes, none received yet.
	StateOpen RFQState = "OPEN"
	// StateQuoted: live with at least one active quote.
	StateQuoted RFQState = "QUOTED"
	// StateExecuted: a quote was accepted and a trade exists. Terminal.
	StateExecuted RFQState = "EXECUTED"
	// StateCancelled: pulled by the client before execution. Terminal.
	StateCancelled RFQState = "CANCELLED"
	// StateExpired: passed its expiry without execution. Terminal.
	StateExpired RFQState = "EXPIRED"
)

// Terminal reports whether the state admits no further transitions.
func (s RFQState) Terminal() bool {
	return s == StateExecuted || s == StateCancelled || s == StateExpired
}

// transitions is the complete set of legal RFQ state changes. Keeping it as
// data rather than branching logic means the state machine can be asserted
// against directly in tests.
var transitions = map[RFQState][]RFQState{
	StateOpen:      {StateQuoted, StateCancelled, StateExpired},
	StateQuoted:    {StateExecuted, StateCancelled, StateExpired},
	StateExecuted:  {},
	StateCancelled: {},
	StateExpired:   {},
}

// ErrIllegalTransition is returned when a state change is not permitted.
var ErrIllegalTransition = errors.New("illegal state transition")

// CanTransition reports whether from -> to is a legal move.
func CanTransition(from, to RFQState) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Collateral identifies the securities pledged against the cash leg.
type Collateral struct {
	CUSIP       string `json:"cusip"`
	Description string `json:"description"`
	AssetClass  string `json:"assetClass"` // UST, AGENCY, CORP, EQUITY
}

// RFQ is a client's request for dealers to price a repo.
type RFQ struct {
	ID         string     `json:"id"`
	ClientID   string     `json:"clientId"`
	Direction  Direction  `json:"direction"`
	Collateral Collateral `json:"collateral"`
	Notional   Money      `json:"notional"` // cash amount, minor units
	Currency   string     `json:"currency"`
	StartDate  time.Time  `json:"startDate"`
	EndDate    time.Time  `json:"endDate"`
	State      RFQState   `json:"state"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	Quotes     []Quote    `json:"quotes,omitempty"`
}

// TermDays is the repo's tenor in days — the accrual period for interest.
func (r *RFQ) TermDays() int {
	return int(r.EndDate.Sub(r.StartDate).Hours() / 24)
}

// Overnight reports whether this is a one-day repo, the most common tenor.
func (r *RFQ) Overnight() bool { return r.TermDays() == 1 }

// Validate checks the RFQ is internally coherent before it reaches the market.
func (r *RFQ) Validate() error {
	if r.ClientID == "" {
		return fmt.Errorf("%w: clientId is required", ErrValidation)
	}
	if !r.Direction.Valid() {
		return fmt.Errorf("%w: direction %q must be REPO or REVERSE_REPO", ErrValidation, r.Direction)
	}
	if r.Notional <= 0 {
		return fmt.Errorf("%w: notional: %w", ErrValidation, ErrNegativeAmount)
	}
	if r.Collateral.CUSIP == "" {
		return fmt.Errorf("%w: collateral cusip is required", ErrValidation)
	}
	if len(r.Currency) != 3 {
		return fmt.Errorf("%w: currency %q must be a 3-letter code", ErrValidation, r.Currency)
	}
	if !r.EndDate.After(r.StartDate) {
		return ErrInvalidTenor
	}
	if r.TermDays() < 1 {
		return ErrInvalidTenor
	}
	return nil
}

// Expired reports whether the RFQ's quote window has closed as of now.
func (r *RFQ) Expired(now time.Time) bool {
	return !r.State.Terminal() && now.After(r.ExpiresAt)
}

// BestQuote returns the quote the client should accept, or nil if no active
// quote exists.
//
// Direction decides the ordering: a repo client pays the rate and wants the
// lowest, a reverse-repo client earns it and wants the highest. Ties break on
// the lower haircut (less collateral tied up), then on the earlier quote, so
// that a dealer who prices first is not overtaken by an identical later quote.
func (r *RFQ) BestQuote() *Quote {
	var best *Quote
	for i := range r.Quotes {
		q := &r.Quotes[i]
		if q.Status != QuoteActive {
			continue
		}
		if best == nil || q.betterThan(best, r.Direction) {
			best = q
		}
	}
	return best
}
