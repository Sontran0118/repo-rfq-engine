package domain

import (
	"fmt"
	"time"
)

// Trade is the booked result of a client accepting a dealer's quote. Economics
// are computed once at execution and stored, so a later change to rate
// conventions cannot silently restate a trade that already settled.
type Trade struct {
	ID                 string      `json:"id"`
	RFQID              string      `json:"rfqId"`
	QuoteID            string      `json:"quoteId"`
	ClientID           string      `json:"clientId"`
	DealerID           string      `json:"dealerId"`
	Direction          Direction   `json:"direction"`
	Collateral         Collateral  `json:"collateral"`
	Notional           Money       `json:"notional"`
	Currency           string      `json:"currency"`
	Rate               BasisPoints `json:"rate"`
	Haircut            BasisPoints `json:"haircut"`
	StartDate          time.Time   `json:"startDate"`
	EndDate            time.Time   `json:"endDate"`
	TermDays           int         `json:"termDays"`
	Interest           Money       `json:"interest"`
	RepurchasePrice    Money       `json:"repurchasePrice"`
	CollateralRequired Money       `json:"collateralRequired"`
	ExecutedAt         time.Time   `json:"executedAt"`
}

// NewTrade books the winning quote against its RFQ, computing the cash legs.
//
// It is the single place where an RFQ turns into an obligation, so it revalidates
// rather than trusting the caller: a quote belonging to a different RFQ, or one
// that is no longer active, must never produce a trade.
func NewTrade(id string, r *RFQ, q *Quote, now time.Time) (*Trade, error) {
	if q.RFQID != r.ID {
		return nil, fmt.Errorf("quote %s belongs to rfq %s, not %s", q.ID, q.RFQID, r.ID)
	}
	if q.Status != QuoteActive {
		return nil, fmt.Errorf("quote %s is %s, not active", q.ID, q.Status)
	}
	if !CanTransition(r.State, StateExecuted) {
		return nil, fmt.Errorf("%w: rfq %s is %s", ErrIllegalTransition, r.ID, r.State)
	}

	days := r.TermDays()
	interest, err := Interest(r.Notional, q.Rate, days)
	if err != nil {
		return nil, err
	}
	collateral, err := CollateralRequired(r.Notional, q.Haircut)
	if err != nil {
		return nil, err
	}

	return &Trade{
		ID:                 id,
		RFQID:              r.ID,
		QuoteID:            q.ID,
		ClientID:           r.ClientID,
		DealerID:           q.DealerID,
		Direction:          r.Direction,
		Collateral:         r.Collateral,
		Notional:           r.Notional,
		Currency:           r.Currency,
		Rate:               q.Rate,
		Haircut:            q.Haircut,
		StartDate:          r.StartDate,
		EndDate:            r.EndDate,
		TermDays:           days,
		Interest:           interest,
		RepurchasePrice:    r.Notional + interest,
		CollateralRequired: collateral,
		ExecutedAt:         now,
	}, nil
}
