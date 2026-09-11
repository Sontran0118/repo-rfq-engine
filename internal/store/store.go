// Package store persists the RFQ lifecycle in MySQL.
//
// The interesting part is AcceptQuote: it is the only operation where two
// clients can race for the same resource, and it is written so that exactly one
// of them wins regardless of how the goroutines interleave.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Sontran0118/repo-rfq-engine/internal/domain"
	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

//go:embed schema.sql
var schemaSQL string

// Sentinel errors the API layer maps onto HTTP status codes.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyTraded = errors.New("rfq has already been executed")
	ErrNotQuotable   = errors.New("rfq is not accepting quotes")
)

// Store is a MySQL-backed repository for RFQs, quotes, and trades.
type Store struct {
	db *sql.DB
	// now is injected so tests can control expiry without sleeping.
	now func() time.Time
}

// New opens a connection pool against dsn and verifies it is reachable.
func New(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	// A repo blotter is bursty: many short transactions, few long ones. Cap the
	// pool so a burst cannot exhaust MySQL's connection limit.
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close releases the connection pool.
func (s *Store) Close() error { return s.db.Close() }

// Migrate applies the schema. Every statement is CREATE TABLE IF NOT EXISTS, so
// it is safe to run on every boot.
func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range splitStatements(schemaSQL) {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// splitStatements breaks a SQL file into individual statements.
//
// Comments are stripped before splitting, not after: a prose comment containing
// a semicolon would otherwise cut the statement it documents in half, producing
// two syntactically invalid fragments.
func splitStatements(sql string) []string {
	var stripped strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		stripped.WriteString(line)
		stripped.WriteString("\n")
	}

	out := []string{}
	for _, stmt := range strings.Split(stripped.String(), ";") {
		if strings.TrimSpace(stmt) != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// TruncateAll clears every table in foreign-key-safe order.
//
// Exported for tests only. Callers must not run it concurrently with other
// tests against the same database — the suite serialises packages for this
// reason.
func (s *Store) TruncateAll(ctx context.Context) error {
	for _, table := range []string{"trades", "quotes", "rfqs", "counterparties"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("truncate %s: %w", table, err)
		}
	}
	return nil
}

// Counterparty is a trading entity — a buy-side client or a sell-side dealer.
type Counterparty struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // CLIENT or DEALER
}

// UpsertCounterparty creates or renames a trading entity.
func (s *Store) UpsertCounterparty(ctx context.Context, c Counterparty) error {
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO counterparties (id, name, kind) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE name = VALUES(name)`,
		c.ID, c.Name, c.Kind)
	return err
}

// ListCounterparties returns all trading entities, clients before dealers.
func (s *Store) ListCounterparties(ctx context.Context) ([]Counterparty, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, kind FROM counterparties ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Counterparty{}
	for rows.Next() {
		var c Counterparty
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateRFQ validates and persists a new request for quote.
func (s *Store) CreateRFQ(ctx context.Context, r *domain.RFQ) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.State == "" {
		r.State = domain.StateOpen
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = s.now().UTC()
	}
	if r.ExpiresAt.IsZero() {
		r.ExpiresAt = r.CreatedAt.Add(5 * time.Minute)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rfqs (id, client_id, direction, collateral_cusip,
		     collateral_desc, collateral_asset_cls, notional, currency,
		     start_date, end_date, state, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ClientID, r.Direction, r.Collateral.CUSIP,
		r.Collateral.Description, r.Collateral.AssetClass, r.Notional, r.Currency,
		r.StartDate, r.EndDate, r.State, r.ExpiresAt, r.CreatedAt)
	return err
}

const rfqColumns = `id, client_id, direction, collateral_cusip, collateral_desc,
	collateral_asset_cls, notional, currency, start_date, end_date, state,
	expires_at, created_at`

func scanRFQ(sc interface{ Scan(...any) error }) (*domain.RFQ, error) {
	var r domain.RFQ
	err := sc.Scan(&r.ID, &r.ClientID, &r.Direction, &r.Collateral.CUSIP,
		&r.Collateral.Description, &r.Collateral.AssetClass, &r.Notional,
		&r.Currency, &r.StartDate, &r.EndDate, &r.State, &r.ExpiresAt, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRFQ returns one RFQ with its quotes attached.
func (s *Store) GetRFQ(ctx context.Context, id string) (*domain.RFQ, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+rfqColumns+` FROM rfqs WHERE id = ?`, id)

	r, err := scanRFQ(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("rfq %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}

	if r.Quotes, err = s.quotesFor(ctx, s.db, r.ID); err != nil {
		return nil, err
	}
	return r, nil
}

// ListRFQs returns RFQs newest first, optionally filtered by state.
func (s *Store) ListRFQs(ctx context.Context, state string, limit int) ([]*domain.RFQ, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	query := `SELECT ` + rfqColumns + ` FROM rfqs`
	args := []any{}
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*domain.RFQ{}
	for rows.Next() {
		r, err := scanRFQ(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attach quotes so the blotter renders best-price without a second round trip.
	for _, r := range out {
		if r.Quotes, err = s.quotesFor(ctx, s.db, r.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) quotesFor(ctx context.Context, q querier, rfqID string) ([]domain.Quote, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, rfq_id, dealer_id, rate_bps, haircut_bps, status, created_at
		 FROM quotes WHERE rfq_id = ? ORDER BY created_at`, rfqID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Quote{}
	for rows.Next() {
		var qt domain.Quote
		if err := rows.Scan(&qt.ID, &qt.RFQID, &qt.DealerID, &qt.Rate,
			&qt.Haircut, &qt.Status, &qt.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, qt)
	}
	return out, rows.Err()
}

// SubmitQuote records a dealer's price and moves the RFQ to QUOTED.
//
// Re-pricing replaces the dealer's existing quote rather than adding a second
// one, which is why the write is an upsert against uq_quote_rfq_dealer.
func (s *Store) SubmitQuote(ctx context.Context, q *domain.Quote) error {
	if err := q.Validate(); err != nil {
		return err
	}
	if q.ID == "" {
		q.ID = uuid.NewString()
	}
	if q.CreatedAt.IsZero() {
		q.CreatedAt = s.now().UTC()
	}
	q.Status = domain.QuoteActive

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock the RFQ so its state cannot change to EXECUTED underneath this quote.
	var state domain.RFQState
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx,
		`SELECT state, expires_at FROM rfqs WHERE id = ? FOR UPDATE`,
		q.RFQID).Scan(&state, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rfq %s: %w", q.RFQID, ErrNotFound)
	}
	if err != nil {
		return err
	}

	if state.Terminal() {
		return fmt.Errorf("rfq %s is %s: %w", q.RFQID, state, ErrNotQuotable)
	}
	if s.now().UTC().After(expiresAt) {
		return fmt.Errorf("rfq %s expired at %s: %w",
			q.RFQID, expiresAt.Format(time.RFC3339), ErrNotQuotable)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO quotes (id, rfq_id, dealer_id, rate_bps, haircut_bps, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		     rate_bps = VALUES(rate_bps),
		     haircut_bps = VALUES(haircut_bps),
		     status = VALUES(status),
		     created_at = VALUES(created_at)`,
		q.ID, q.RFQID, q.DealerID, q.Rate, q.Haircut, q.Status, q.CreatedAt)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1452 { // FK violation
			return fmt.Errorf("dealer %s: %w", q.DealerID, ErrNotFound)
		}
		return err
	}

	if state == domain.StateOpen {
		if _, err := tx.ExecContext(ctx,
			`UPDATE rfqs SET state = ? WHERE id = ?`,
			domain.StateQuoted, q.RFQID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AcceptQuote books the winning quote as a trade.
//
// This is the one place in the system where a race has real consequences: two
// concurrent accepts on the same RFQ must not produce two trades, because that
// would double the client's obligation. Three things enforce that, in order:
//
//  1. SELECT ... FOR UPDATE takes an exclusive row lock on the RFQ. The second
//     caller blocks here until the first commits.
//  2. On waking, the second caller re-reads state and finds EXECUTED, so it
//     returns ErrAlreadyTraded instead of proceeding.
//  3. trades.rfq_id is UNIQUE, so even a logic bug above cannot write a second
//     trade row.
//
// Losing quotes are marked LOST in the same transaction, so the book is never
// observably in a state where a trade exists but the dealers do not know.
func (s *Store) AcceptQuote(ctx context.Context, rfqID, quoteID string) (*domain.Trade, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx,
		`SELECT `+rfqColumns+` FROM rfqs WHERE id = ? FOR UPDATE`, rfqID)
	r, err := scanRFQ(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("rfq %s: %w", rfqID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}

	// Re-read after acquiring the lock: another caller may have executed while
	// this one waited.
	if r.State == domain.StateExecuted {
		return nil, fmt.Errorf("rfq %s: %w", rfqID, ErrAlreadyTraded)
	}
	if r.State.Terminal() {
		return nil, fmt.Errorf("rfq %s is %s: %w", rfqID, r.State, ErrNotQuotable)
	}
	if s.now().UTC().After(r.ExpiresAt) {
		return nil, fmt.Errorf("rfq %s expired: %w", rfqID, ErrNotQuotable)
	}

	if r.Quotes, err = s.quotesFor(ctx, tx, rfqID); err != nil {
		return nil, err
	}

	// An empty quoteID means "give me the best price", which is how the blotter's
	// one-click accept works.
	var winner *domain.Quote
	if quoteID == "" {
		winner = r.BestQuote()
		if winner == nil {
			return nil, fmt.Errorf("rfq %s has no active quotes: %w", rfqID, ErrNotQuotable)
		}
	} else {
		for i := range r.Quotes {
			if r.Quotes[i].ID == quoteID {
				winner = &r.Quotes[i]
				break
			}
		}
		if winner == nil {
			return nil, fmt.Errorf("quote %s: %w", quoteID, ErrNotFound)
		}
	}

	trade, err := domain.NewTrade(uuid.NewString(), r, winner, s.now().UTC())
	if err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO trades (id, rfq_id, quote_id, client_id, dealer_id, direction,
		     collateral_cusip, notional, currency, rate_bps, haircut_bps,
		     start_date, end_date, term_days, interest, repurchase_price,
		     collateral_required, executed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		trade.ID, trade.RFQID, trade.QuoteID, trade.ClientID, trade.DealerID,
		trade.Direction, trade.Collateral.CUSIP, trade.Notional, trade.Currency,
		trade.Rate, trade.Haircut, trade.StartDate, trade.EndDate, trade.TermDays,
		trade.Interest, trade.RepurchasePrice, trade.CollateralRequired,
		trade.ExecutedAt,
	); err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 { // duplicate on uq_trade_rfq
			return nil, fmt.Errorf("rfq %s: %w", rfqID, ErrAlreadyTraded)
		}
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE quotes SET status = CASE WHEN id = ? THEN 'WON' ELSE 'LOST' END
		 WHERE rfq_id = ? AND status = 'ACTIVE'`,
		winner.ID, rfqID); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE rfqs SET state = ? WHERE id = ?`,
		domain.StateExecuted, rfqID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return trade, nil
}

// CancelRFQ withdraws a live request before it trades.
func (s *Store) CancelRFQ(ctx context.Context, rfqID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE rfqs SET state = 'CANCELLED'
		 WHERE id = ? AND state IN ('OPEN', 'QUOTED')`, rfqID)
	if err != nil {
		return err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Either it does not exist or it is already terminal; distinguish the two
		// so the caller can return 404 rather than a misleading 409.
		var state domain.RFQState
		err := s.db.QueryRowContext(ctx,
			`SELECT state FROM rfqs WHERE id = ?`, rfqID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rfq %s: %w", rfqID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("rfq %s is %s: %w", rfqID, state, ErrNotQuotable)
	}
	return nil
}

// ExpireStale marks live RFQs past their expiry as EXPIRED and reports how many
// were swept. Called on a ticker by the server.
func (s *Store) ExpireStale(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE rfqs SET state = 'EXPIRED'
		 WHERE state IN ('OPEN', 'QUOTED') AND expires_at < ?`, s.now().UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListTrades returns booked trades, most recent first.
func (s *Store) ListTrades(ctx context.Context, limit int) ([]*domain.Trade, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, rfq_id, quote_id, client_id, dealer_id, direction,
		     collateral_cusip, notional, currency, rate_bps, haircut_bps,
		     start_date, end_date, term_days, interest, repurchase_price,
		     collateral_required, executed_at
		 FROM trades ORDER BY executed_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*domain.Trade{}
	for rows.Next() {
		var t domain.Trade
		if err := rows.Scan(&t.ID, &t.RFQID, &t.QuoteID, &t.ClientID, &t.DealerID,
			&t.Direction, &t.Collateral.CUSIP, &t.Notional, &t.Currency, &t.Rate,
			&t.Haircut, &t.StartDate, &t.EndDate, &t.TermDays, &t.Interest,
			&t.RepurchasePrice, &t.CollateralRequired, &t.ExecutedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}
