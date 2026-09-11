package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Sontran0118/repo-rfq-engine/internal/domain"
)

// testDSN points at a throwaway database. Integration tests skip rather than
// fail when MySQL is unavailable, so `go test ./...` still works on a machine
// without a server — but CI sets RFQ_TEST_DSN and they run for real.
func testDSN() string {
	if dsn := os.Getenv("RFQ_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "rfq:rfq@tcp(127.0.0.1:3306)/rfq_engine_test?parseTime=true&multiStatements=true&loc=UTC"
}

func newTestStore(t *testing.T) *Store {
	t.Helper()

	ctx := context.Background()
	s, err := New(ctx, testDSN())
	if err != nil {
		t.Skipf("MySQL unavailable, skipping integration test: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	if err := s.TruncateAll(ctx); err != nil {
		t.Fatalf("clean: %v", err)
	}

	t.Cleanup(func() { s.Close() })
	return s
}

func seedCounterparties(t *testing.T, s *Store, dealers ...string) {
	t.Helper()
	ctx := context.Background()

	if err := s.UpsertCounterparty(ctx, Counterparty{
		ID: "client-a", Name: "Pension Fund A", Kind: "CLIENT",
	}); err != nil {
		t.Fatalf("seed client: %v", err)
	}
	for _, d := range dealers {
		if err := s.UpsertCounterparty(ctx, Counterparty{
			ID: d, Name: d, Kind: "DEALER",
		}); err != nil {
			t.Fatalf("seed dealer %s: %v", d, err)
		}
	}
}

func newRFQ() *domain.RFQ {
	start := time.Now().UTC().Truncate(24 * time.Hour)
	return &domain.RFQ{
		ClientID:  "client-a",
		Direction: domain.Repo,
		Collateral: domain.Collateral{
			CUSIP: "912810TM0", Description: "US TREASURY N/B", AssetClass: "UST",
		},
		Notional:  1_000_000_00, // $1,000,000.00
		Currency:  "USD",
		StartDate: start,
		EndDate:   start.AddDate(0, 0, 1),
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}
}

func TestCreateAndGetRFQ(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s)

	r := newRFQ()
	if err := s.CreateRFQ(ctx, r); err != nil {
		t.Fatalf("CreateRFQ() error = %v", err)
	}
	if r.ID == "" {
		t.Fatal("CreateRFQ() did not assign an ID")
	}

	got, err := s.GetRFQ(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRFQ() error = %v", err)
	}
	if got.State != domain.StateOpen {
		t.Errorf("State = %s, want OPEN", got.State)
	}
	if got.Notional != r.Notional {
		t.Errorf("Notional = %s, want %s", got.Notional, r.Notional)
	}
	if got.Collateral.CUSIP != "912810TM0" {
		t.Errorf("CUSIP = %q, want 912810TM0", got.Collateral.CUSIP)
	}
}

func TestGetRFQNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetRFQ(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRFQ() error = %v, want ErrNotFound", err)
	}
}

func TestCreateRFQRejectsInvalid(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s)

	r := newRFQ()
	r.Notional = -1
	if err := s.CreateRFQ(ctx, r); err == nil {
		t.Error("CreateRFQ() accepted a negative notional")
	}
}

func TestSubmitQuoteMovesToQuoted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s, "dealer-a")

	r := newRFQ()
	if err := s.CreateRFQ(ctx, r); err != nil {
		t.Fatalf("CreateRFQ() error = %v", err)
	}

	q := &domain.Quote{RFQID: r.ID, DealerID: "dealer-a", Rate: 525, Haircut: 200}
	if err := s.SubmitQuote(ctx, q); err != nil {
		t.Fatalf("SubmitQuote() error = %v", err)
	}

	got, err := s.GetRFQ(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRFQ() error = %v", err)
	}
	if got.State != domain.StateQuoted {
		t.Errorf("State = %s, want QUOTED", got.State)
	}
	if len(got.Quotes) != 1 {
		t.Fatalf("got %d quotes, want 1", len(got.Quotes))
	}
	if got.Quotes[0].Rate != 525 {
		t.Errorf("Rate = %s, want 5.25%%", got.Quotes[0].Rate)
	}
}

// TestRepricingReplacesQuote guards the uq_quote_rfq_dealer constraint. A dealer
// who improves their price must not end up with two live quotes, or the same
// dealer could occupy both sides of best-price selection.
func TestRepricingReplacesQuote(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s, "dealer-a")

	r := newRFQ()
	if err := s.CreateRFQ(ctx, r); err != nil {
		t.Fatalf("CreateRFQ() error = %v", err)
	}

	for _, rate := range []domain.BasisPoints{530, 515} {
		q := &domain.Quote{RFQID: r.ID, DealerID: "dealer-a", Rate: rate, Haircut: 200}
		if err := s.SubmitQuote(ctx, q); err != nil {
			t.Fatalf("SubmitQuote(%s) error = %v", rate, err)
		}
	}

	got, err := s.GetRFQ(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRFQ() error = %v", err)
	}
	if len(got.Quotes) != 1 {
		t.Fatalf("got %d quotes after repricing, want 1", len(got.Quotes))
	}
	if got.Quotes[0].Rate != 515 {
		t.Errorf("Rate = %s, want the improved 5.15%%", got.Quotes[0].Rate)
	}
}

func TestAcceptQuoteBooksTrade(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s, "dealer-a", "dealer-b")

	r := newRFQ()
	if err := s.CreateRFQ(ctx, r); err != nil {
		t.Fatalf("CreateRFQ() error = %v", err)
	}
	for dealer, rate := range map[string]domain.BasisPoints{"dealer-a": 530, "dealer-b": 515} {
		if err := s.SubmitQuote(ctx, &domain.Quote{
			RFQID: r.ID, DealerID: dealer, Rate: rate, Haircut: 200,
		}); err != nil {
			t.Fatalf("SubmitQuote(%s) error = %v", dealer, err)
		}
	}

	// Empty quoteID means "accept the best price".
	trade, err := s.AcceptQuote(ctx, r.ID, "")
	if err != nil {
		t.Fatalf("AcceptQuote() error = %v", err)
	}

	// Repo client pays, so the 5.15% quote from dealer-b must win.
	if trade.DealerID != "dealer-b" {
		t.Errorf("DealerID = %s, want dealer-b at the lower rate", trade.DealerID)
	}
	if trade.Rate != 515 {
		t.Errorf("Rate = %s, want 5.15%%", trade.Rate)
	}

	// $1m overnight at 5.15% ACT/360 = $143.06.
	if trade.Interest != 14_306 {
		t.Errorf("Interest = %s, want 143.06", trade.Interest)
	}
	// 2% haircut on $1m = $1,020,000.
	if trade.CollateralRequired != 1_020_000_00 {
		t.Errorf("CollateralRequired = %s, want 1020000.00", trade.CollateralRequired)
	}

	got, err := s.GetRFQ(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRFQ() error = %v", err)
	}
	if got.State != domain.StateExecuted {
		t.Errorf("State = %s, want EXECUTED", got.State)
	}

	// The winner is marked WON and every other active quote LOST, in the same
	// transaction that booked the trade.
	var won, lost int
	for _, q := range got.Quotes {
		switch q.Status {
		case domain.QuoteWon:
			won++
		case domain.QuoteLost:
			lost++
		}
	}
	if won != 1 || lost != 1 {
		t.Errorf("quote statuses: %d WON / %d LOST, want 1 / 1", won, lost)
	}
}

// TestAcceptQuoteIsExactlyOnce is the reason this service uses a row lock rather
// than an optimistic read. Twenty goroutines race to accept the same RFQ; the
// invariant is that exactly one trade exists afterwards and every other caller
// is told why it lost. A double-book here would double a client's real cash
// obligation.
func TestAcceptQuoteIsExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s, "dealer-a")

	r := newRFQ()
	if err := s.CreateRFQ(ctx, r); err != nil {
		t.Fatalf("CreateRFQ() error = %v", err)
	}
	if err := s.SubmitQuote(ctx, &domain.Quote{
		RFQID: r.ID, DealerID: "dealer-a", Rate: 525, Haircut: 200,
	}); err != nil {
		t.Fatalf("SubmitQuote() error = %v", err)
	}

	const racers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		trades  []*domain.Trade
		refused int
		other   []error
		gate    = make(chan struct{})
	)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate // release all goroutines at once to maximise contention

			trade, err := s.AcceptQuote(ctx, r.ID, "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				trades = append(trades, trade)
			case errors.Is(err, ErrAlreadyTraded):
				refused++
			default:
				other = append(other, err)
			}
		}()
	}

	close(gate)
	wg.Wait()

	if len(trades) != 1 {
		t.Errorf("booked %d trades, want exactly 1", len(trades))
	}
	if refused != racers-1 {
		t.Errorf("%d callers got ErrAlreadyTraded, want %d", refused, racers-1)
	}
	if len(other) > 0 {
		t.Errorf("unexpected errors: %v", other)
	}

	// The database must agree with what the callers were told.
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM trades WHERE rfq_id = ?`, r.ID).Scan(&count); err != nil {
		t.Fatalf("count trades: %v", err)
	}
	if count != 1 {
		t.Errorf("trades table holds %d rows for this rfq, want 1", count)
	}
}

func TestAcceptQuoteRejects(t *testing.T) {
	ctx := context.Background()

	t.Run("rfq with no quotes", func(t *testing.T) {
		s := newTestStore(t)
		seedCounterparties(t, s)

		r := newRFQ()
		if err := s.CreateRFQ(ctx, r); err != nil {
			t.Fatalf("CreateRFQ() error = %v", err)
		}
		if _, err := s.AcceptQuote(ctx, r.ID, ""); !errors.Is(err, ErrNotQuotable) {
			t.Errorf("AcceptQuote() error = %v, want ErrNotQuotable", err)
		}
	})

	t.Run("unknown rfq", func(t *testing.T) {
		s := newTestStore(t)
		if _, err := s.AcceptQuote(ctx, "nope", ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("AcceptQuote() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("cancelled rfq", func(t *testing.T) {
		s := newTestStore(t)
		seedCounterparties(t, s, "dealer-a")

		r := newRFQ()
		if err := s.CreateRFQ(ctx, r); err != nil {
			t.Fatalf("CreateRFQ() error = %v", err)
		}
		if err := s.SubmitQuote(ctx, &domain.Quote{
			RFQID: r.ID, DealerID: "dealer-a", Rate: 525, Haircut: 200,
		}); err != nil {
			t.Fatalf("SubmitQuote() error = %v", err)
		}
		if err := s.CancelRFQ(ctx, r.ID); err != nil {
			t.Fatalf("CancelRFQ() error = %v", err)
		}

		if _, err := s.AcceptQuote(ctx, r.ID, ""); !errors.Is(err, ErrNotQuotable) {
			t.Errorf("AcceptQuote() error = %v, want ErrNotQuotable", err)
		}
	})
}

// TestQuotingExpiredRFQ checks the clock is respected on the write path, not
// just at read time — a quote landing after expiry must be refused.
func TestQuotingExpiredRFQ(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s, "dealer-a")

	r := newRFQ()
	r.ExpiresAt = time.Now().UTC().Add(-time.Minute) // already closed
	if err := s.CreateRFQ(ctx, r); err != nil {
		t.Fatalf("CreateRFQ() error = %v", err)
	}

	err := s.SubmitQuote(ctx, &domain.Quote{
		RFQID: r.ID, DealerID: "dealer-a", Rate: 525, Haircut: 200,
	})
	if !errors.Is(err, ErrNotQuotable) {
		t.Errorf("SubmitQuote() error = %v, want ErrNotQuotable", err)
	}
}

func TestExpireStale(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s)

	stale := newRFQ()
	stale.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := s.CreateRFQ(ctx, stale); err != nil {
		t.Fatalf("CreateRFQ(stale) error = %v", err)
	}

	live := newRFQ()
	if err := s.CreateRFQ(ctx, live); err != nil {
		t.Fatalf("CreateRFQ(live) error = %v", err)
	}

	n, err := s.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("ExpireStale() error = %v", err)
	}
	if n != 1 {
		t.Errorf("ExpireStale() swept %d, want 1", n)
	}

	got, _ := s.GetRFQ(ctx, stale.ID)
	if got.State != domain.StateExpired {
		t.Errorf("stale RFQ state = %s, want EXPIRED", got.State)
	}
	got, _ = s.GetRFQ(ctx, live.ID)
	if got.State != domain.StateOpen {
		t.Errorf("live RFQ state = %s, want OPEN", got.State)
	}
}

func TestCancelRFQ(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s)

	r := newRFQ()
	if err := s.CreateRFQ(ctx, r); err != nil {
		t.Fatalf("CreateRFQ() error = %v", err)
	}
	if err := s.CancelRFQ(ctx, r.ID); err != nil {
		t.Fatalf("CancelRFQ() error = %v", err)
	}

	// Cancelling twice must report the conflict rather than silently succeeding.
	if err := s.CancelRFQ(ctx, r.ID); !errors.Is(err, ErrNotQuotable) {
		t.Errorf("second CancelRFQ() error = %v, want ErrNotQuotable", err)
	}
	if err := s.CancelRFQ(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CancelRFQ(unknown) error = %v, want ErrNotFound", err)
	}
}

func TestListRFQsAndTrades(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedCounterparties(t, s, "dealer-a")

	for i := 0; i < 3; i++ {
		r := newRFQ()
		if err := s.CreateRFQ(ctx, r); err != nil {
			t.Fatalf("CreateRFQ() error = %v", err)
		}
		if i == 0 {
			if err := s.SubmitQuote(ctx, &domain.Quote{
				RFQID: r.ID, DealerID: "dealer-a", Rate: 525, Haircut: 200,
			}); err != nil {
				t.Fatalf("SubmitQuote() error = %v", err)
			}
			if _, err := s.AcceptQuote(ctx, r.ID, ""); err != nil {
				t.Fatalf("AcceptQuote() error = %v", err)
			}
		}
	}

	all, err := s.ListRFQs(ctx, "", 0)
	if err != nil {
		t.Fatalf("ListRFQs() error = %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ListRFQs() returned %d, want 3", len(all))
	}

	open, err := s.ListRFQs(ctx, "OPEN", 0)
	if err != nil {
		t.Fatalf("ListRFQs(OPEN) error = %v", err)
	}
	if len(open) != 2 {
		t.Errorf("ListRFQs(OPEN) returned %d, want 2", len(open))
	}

	trades, err := s.ListTrades(ctx, 0)
	if err != nil {
		t.Fatalf("ListTrades() error = %v", err)
	}
	if len(trades) != 1 {
		t.Errorf("ListTrades() returned %d, want 1", len(trades))
	}
}
