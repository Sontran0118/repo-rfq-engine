package domain

import (
	"errors"
	"testing"
	"time"
)

var (
	start = time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	next  = start.AddDate(0, 0, 1)
)

func newRFQ(d Direction) *RFQ {
	return &RFQ{
		ID:        "rfq-1",
		ClientID:  "client-a",
		Direction: d,
		Collateral: Collateral{
			CUSIP:       "912810TM0",
			Description: "US TREASURY N/B 4.25% 2054",
			AssetClass:  "UST",
		},
		Notional:  10 * million,
		Currency:  "USD",
		StartDate: start,
		EndDate:   next,
		State:     StateOpen,
		ExpiresAt: start.Add(5 * time.Minute),
		CreatedAt: start,
	}
}

func quote(id, dealer string, rate, haircut BasisPoints, at time.Time) Quote {
	return Quote{
		ID:        id,
		RFQID:     "rfq-1",
		DealerID:  dealer,
		Rate:      rate,
		Haircut:   haircut,
		Status:    QuoteActive,
		CreatedAt: at,
	}
}

// TestStateMachine asserts the full transition table, legal and illegal moves
// alike. An RFQ that can be executed twice books two trades against one request,
// so the negative cases matter more than the positive ones.
func TestStateMachine(t *testing.T) {
	all := []RFQState{StateOpen, StateQuoted, StateExecuted, StateCancelled, StateExpired}

	legal := map[RFQState]map[RFQState]bool{
		StateOpen:   {StateQuoted: true, StateCancelled: true, StateExpired: true},
		StateQuoted: {StateExecuted: true, StateCancelled: true, StateExpired: true},
	}

	for _, from := range all {
		for _, to := range all {
			want := legal[from][to]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalStatesAreFinal(t *testing.T) {
	for _, s := range []RFQState{StateExecuted, StateCancelled, StateExpired} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
		for _, to := range []RFQState{StateOpen, StateQuoted, StateExecuted} {
			if CanTransition(s, to) {
				t.Errorf("%s -> %s should be illegal from a terminal state", s, to)
			}
		}
	}
	for _, s := range []RFQState{StateOpen, StateQuoted} {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

// TestBestQuoteDirection is the core domain rule: which quote wins depends on
// which side of the cash the client is on. Getting this backwards would hand
// the client the worst price in the book while reporting it as the best.
func TestBestQuoteDirection(t *testing.T) {
	quotes := []Quote{
		quote("q1", "dealer-a", 530, 200, start),
		quote("q2", "dealer-b", 515, 200, start.Add(time.Second)),
		quote("q3", "dealer-c", 545, 200, start.Add(2*time.Second)),
	}

	t.Run("repo client pays, so lowest rate wins", func(t *testing.T) {
		r := newRFQ(Repo)
		r.Quotes = quotes
		best := r.BestQuote()
		if best == nil || best.ID != "q2" {
			t.Fatalf("BestQuote() = %v, want q2 at 5.15%%", best)
		}
	})

	t.Run("reverse repo client earns, so highest rate wins", func(t *testing.T) {
		r := newRFQ(ReverseRepo)
		r.Quotes = quotes
		best := r.BestQuote()
		if best == nil || best.ID != "q3" {
			t.Fatalf("BestQuote() = %v, want q3 at 5.45%%", best)
		}
	})
}

func TestBestQuoteTieBreaks(t *testing.T) {
	t.Run("equal rates break on the lower haircut", func(t *testing.T) {
		r := newRFQ(Repo)
		r.Quotes = []Quote{
			quote("q1", "dealer-a", 525, 200, start),
			quote("q2", "dealer-b", 525, 50, start.Add(time.Second)),
		}
		if best := r.BestQuote(); best == nil || best.ID != "q2" {
			t.Fatalf("BestQuote() = %v, want q2 with the 0.50%% haircut", best)
		}
	})

	t.Run("identical quotes break on who priced first", func(t *testing.T) {
		r := newRFQ(Repo)
		r.Quotes = []Quote{
			quote("q_late", "dealer-a", 525, 200, start.Add(time.Second)),
			quote("q_first", "dealer-b", 525, 200, start),
		}
		if best := r.BestQuote(); best == nil || best.ID != "q_first" {
			t.Fatalf("BestQuote() = %v, want the earlier quote", best)
		}
	})
}

func TestBestQuoteIgnoresInactive(t *testing.T) {
	r := newRFQ(Repo)
	withdrawn := quote("q_withdrawn", "dealer-a", 400, 200, start)
	withdrawn.Status = QuoteWithdrawn
	lost := quote("q_lost", "dealer-b", 410, 200, start)
	lost.Status = QuoteLost

	r.Quotes = []Quote{withdrawn, lost, quote("q_active", "dealer-c", 525, 200, start)}

	// The withdrawn quote is the best price in the list; it must not be chosen.
	if best := r.BestQuote(); best == nil || best.ID != "q_active" {
		t.Fatalf("BestQuote() = %v, want the only active quote", best)
	}
}

func TestBestQuoteEmpty(t *testing.T) {
	r := newRFQ(Repo)
	if best := r.BestQuote(); best != nil {
		t.Errorf("BestQuote() = %v, want nil when no quotes exist", best)
	}
}

func TestRFQValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*RFQ)
		wantErr bool
	}{
		{"valid", func(*RFQ) {}, false},
		{"no client", func(r *RFQ) { r.ClientID = "" }, true},
		{"bad direction", func(r *RFQ) { r.Direction = "SWAP" }, true},
		{"zero notional", func(r *RFQ) { r.Notional = 0 }, true},
		{"negative notional", func(r *RFQ) { r.Notional = -million }, true},
		{"no cusip", func(r *RFQ) { r.Collateral.CUSIP = "" }, true},
		{"bad currency", func(r *RFQ) { r.Currency = "DOLLAR" }, true},
		{"end before start", func(r *RFQ) { r.EndDate = r.StartDate.AddDate(0, 0, -1) }, true},
		{"same day", func(r *RFQ) { r.EndDate = r.StartDate }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRFQ(Repo)
			tc.mutate(r)
			err := r.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestTermDaysAndOvernight(t *testing.T) {
	r := newRFQ(Repo)
	if got := r.TermDays(); got != 1 {
		t.Errorf("TermDays() = %d, want 1", got)
	}
	if !r.Overnight() {
		t.Error("Overnight() = false, want true for a one-day repo")
	}

	r.EndDate = start.AddDate(0, 0, 30)
	if got := r.TermDays(); got != 30 {
		t.Errorf("TermDays() = %d, want 30", got)
	}
	if r.Overnight() {
		t.Error("Overnight() = true, want false for a 30-day term")
	}
}

func TestExpired(t *testing.T) {
	r := newRFQ(Repo)
	if r.Expired(start) {
		t.Error("Expired() = true at creation time")
	}
	if !r.Expired(r.ExpiresAt.Add(time.Second)) {
		t.Error("Expired() = false after the quote window closed")
	}

	// A trade that already executed does not retroactively expire.
	r.State = StateExecuted
	if r.Expired(r.ExpiresAt.Add(time.Hour)) {
		t.Error("Expired() = true for an executed RFQ")
	}
}

func TestQuoteValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Quote)
		wantErr bool
	}{
		{"valid", func(*Quote) {}, false},
		{"no dealer", func(q *Quote) { q.DealerID = "" }, true},
		{"negative haircut", func(q *Quote) { q.Haircut = -100 }, true},
		{"implausible rate", func(q *Quote) { q.Rate = 20_000 }, true},
		// Special collateral genuinely trades negative; this must be allowed.
		{"negative rate on special", func(q *Quote) { q.Rate = -25 }, false},
		{"zero rate", func(q *Quote) { q.Rate = 0 }, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := quote("q1", "dealer-a", 525, 200, start)
			tc.mutate(&q)
			err := q.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewTrade(t *testing.T) {
	r := newRFQ(Repo)
	r.State = StateQuoted
	q := quote("q1", "dealer-b", 525, 200, start)
	r.Quotes = []Quote{q}

	trade, err := NewTrade("trade-1", r, &q, start)
	if err != nil {
		t.Fatalf("NewTrade() error = %v", err)
	}

	if trade.Interest != 145_833 {
		t.Errorf("Interest = %s, want 1458.33", trade.Interest)
	}
	if trade.RepurchasePrice != 10*million+145_833 {
		t.Errorf("RepurchasePrice = %s, want %s", trade.RepurchasePrice, 10*million+145_833)
	}
	if trade.CollateralRequired != 1_020_000_000 {
		t.Errorf("CollateralRequired = %s, want 10200000.00", trade.CollateralRequired)
	}
	if trade.TermDays != 1 {
		t.Errorf("TermDays = %d, want 1", trade.TermDays)
	}
	if trade.DealerID != "dealer-b" || trade.ClientID != "client-a" {
		t.Errorf("counterparties = %s/%s, want client-a/dealer-b", trade.ClientID, trade.DealerID)
	}
}

// TestNewTradeRejects covers the ways a trade must refuse to book. Each of these
// would otherwise create an obligation the client never agreed to.
func TestNewTradeRejects(t *testing.T) {
	t.Run("quote from a different rfq", func(t *testing.T) {
		r := newRFQ(Repo)
		r.State = StateQuoted
		q := quote("q1", "dealer-b", 525, 200, start)
		q.RFQID = "rfq-other"

		if _, err := NewTrade("t1", r, &q, start); err == nil {
			t.Error("NewTrade() accepted a quote belonging to another RFQ")
		}
	})

	t.Run("withdrawn quote", func(t *testing.T) {
		r := newRFQ(Repo)
		r.State = StateQuoted
		q := quote("q1", "dealer-b", 525, 200, start)
		q.Status = QuoteWithdrawn

		if _, err := NewTrade("t1", r, &q, start); err == nil {
			t.Error("NewTrade() accepted a withdrawn quote")
		}
	})

	t.Run("already executed rfq", func(t *testing.T) {
		r := newRFQ(Repo)
		r.State = StateExecuted
		q := quote("q1", "dealer-b", 525, 200, start)

		_, err := NewTrade("t1", r, &q, start)
		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("NewTrade() error = %v, want ErrIllegalTransition", err)
		}
	})

	t.Run("cancelled rfq", func(t *testing.T) {
		r := newRFQ(Repo)
		r.State = StateCancelled
		q := quote("q1", "dealer-b", 525, 200, start)

		if _, err := NewTrade("t1", r, &q, start); err == nil {
			t.Error("NewTrade() booked a trade against a cancelled RFQ")
		}
	})

	t.Run("open rfq with no quotes cannot execute", func(t *testing.T) {
		r := newRFQ(Repo)
		q := quote("q1", "dealer-b", 525, 200, start)

		// OPEN -> EXECUTED skips the QUOTED state and must be refused.
		if _, err := NewTrade("t1", r, &q, start); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("NewTrade() error = %v, want ErrIllegalTransition", err)
		}
	})
}
