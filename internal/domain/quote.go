package domain

import (
	"fmt"
	"time"
)

// QuoteStatus tracks a dealer's price through the RFQ's life.
type QuoteStatus string

const (
	// QuoteActive: live and acceptable by the client.
	QuoteActive QuoteStatus = "ACTIVE"
	// QuoteWithdrawn: pulled by the dealer before acceptance.
	QuoteWithdrawn QuoteStatus = "WITHDRAWN"
	// QuoteWon: accepted; a trade was booked against it.
	QuoteWon QuoteStatus = "WON"
	// QuoteLost: another dealer's quote won the RFQ.
	QuoteLost QuoteStatus = "LOST"
)

// Quote is a dealer's price on an RFQ.
type Quote struct {
	ID        string      `json:"id"`
	RFQID     string      `json:"rfqId"`
	DealerID  string      `json:"dealerId"`
	Rate      BasisPoints `json:"rate"`    // repo rate in bps
	Haircut   BasisPoints `json:"haircut"` // collateral margin in bps
	Status    QuoteStatus `json:"status"`
	CreatedAt time.Time   `json:"createdAt"`
}

// Validate checks a quote is well formed. Negative repo rates are deliberately
// permitted: they occur in practice when specific collateral goes "special" and
// lenders accept a negative return to secure it.
func (q *Quote) Validate() error {
	if q.DealerID == "" {
		return fmt.Errorf("%w: dealerId is required", ErrValidation)
	}
	if q.Haircut < 0 {
		return fmt.Errorf("%w: haircut: %w", ErrValidation, ErrNegativeAmount)
	}
	if q.Rate < -bpsPerUnit || q.Rate > bpsPerUnit {
		return fmt.Errorf("%w: rate %s is outside the plausible range of -100%% to 100%%", ErrValidation, q.Rate)
	}
	return nil
}

// betterThan reports whether q prices more attractively than other, from the
// perspective of the client who raised the RFQ.
func (q *Quote) betterThan(other *Quote, d Direction) bool {
	if q.Rate != other.Rate {
		if d == Repo {
			return q.Rate < other.Rate // borrower pays: lower is better
		}
		return q.Rate > other.Rate // lender earns: higher is better
	}
	if q.Haircut != other.Haircut {
		return q.Haircut < other.Haircut // less collateral tied up
	}
	return q.CreatedAt.Before(other.CreatedAt) // first to price wins
}
