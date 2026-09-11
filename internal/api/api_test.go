package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sontran0118/repo-rfq-engine/internal/domain"
	"github.com/Sontran0118/repo-rfq-engine/internal/store"
)

func testDSN() string {
	if dsn := os.Getenv("RFQ_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "rfq:rfq@tcp(127.0.0.1:3306)/rfq_engine_test?parseTime=true&loc=UTC"
}

func newTestServer(t *testing.T) http.Handler {
	t.Helper()

	ctx := context.Background()
	st, err := store.New(ctx, testDSN())
	if err != nil {
		t.Skipf("MySQL unavailable, skipping API integration test: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Clear in FK-safe order so each test starts from a known book. This shares
	// a database with the store package's tests, which is why the suite runs
	// with -p 1 — see the Makefile.
	if err := st.TruncateAll(ctx); err != nil {
		t.Fatalf("clean: %v", err)
	}

	srv := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Routes()

	// Seed the counterparties every test needs.
	for _, body := range []string{
		`{"id":"client-a","name":"Client A","kind":"CLIENT"}`,
		`{"id":"dealer-a","name":"Dealer A","kind":"DEALER"}`,
		`{"id":"dealer-b","name":"Dealer B","kind":"DEALER"}`,
	} {
		if rec := do(h, "POST", "/api/counterparties", body); rec.Code != http.StatusCreated {
			t.Fatalf("seed counterparty: status %d, body %s", rec.Code, rec.Body)
		}
	}
	return h
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func createRFQBody(notional int64) string {
	return `{"clientId":"client-a","direction":"REPO","notional":` +
		json.Number(itoa(notional)).String() +
		`,"currency":"USD","termDays":1,"collateral":{"cusip":"912810TM0","description":"UST","assetClass":"UST"}}`
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

func TestHealth(t *testing.T) {
	h := newTestServer(t)
	if rec := do(h, "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestFullLifecycle walks one RFQ from creation to a booked trade over HTTP,
// asserting the economics that come back on the wire.
func TestFullLifecycle(t *testing.T) {
	h := newTestServer(t)

	rec := do(h, "POST", "/api/rfqs", createRFQBody(1_000_000_00))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create rfq: status %d, body %s", rec.Code, rec.Body)
	}
	rfq := decodeBody[domain.RFQ](t, rec)
	if rfq.State != domain.StateOpen {
		t.Errorf("state = %s, want OPEN", rfq.State)
	}

	for _, q := range []string{
		`{"dealerId":"dealer-a","rate":530,"haircut":200}`,
		`{"dealerId":"dealer-b","rate":515,"haircut":200}`,
	} {
		if rec := do(h, "POST", "/api/rfqs/"+rfq.ID+"/quotes", q); rec.Code != http.StatusCreated {
			t.Fatalf("submit quote: status %d, body %s", rec.Code, rec.Body)
		}
	}

	rec = do(h, "GET", "/api/rfqs/"+rfq.ID, "")
	quoted := decodeBody[domain.RFQ](t, rec)
	if quoted.State != domain.StateQuoted {
		t.Errorf("state = %s, want QUOTED", quoted.State)
	}
	if len(quoted.Quotes) != 2 {
		t.Errorf("got %d quotes, want 2", len(quoted.Quotes))
	}

	rec = do(h, "POST", "/api/rfqs/"+rfq.ID+"/accept", `{"quoteId":""}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("accept: status %d, body %s", rec.Code, rec.Body)
	}
	trade := decodeBody[domain.Trade](t, rec)

	// A repo client pays the rate, so the cheaper dealer-b quote must win.
	if trade.DealerID != "dealer-b" {
		t.Errorf("dealerId = %s, want dealer-b", trade.DealerID)
	}
	if trade.Interest != 14_306 { // $1m x 5.15% x 1/360
		t.Errorf("interest = %s, want 143.06", trade.Interest)
	}
	if trade.CollateralRequired != 1_020_000_00 { // 2% haircut
		t.Errorf("collateralRequired = %s, want 1020000.00", trade.CollateralRequired)
	}

	// Accepting twice must be a 409, not a 500 — the UI treats them differently.
	if rec := do(h, "POST", "/api/rfqs/"+rfq.ID+"/accept", ""); rec.Code != http.StatusConflict {
		t.Errorf("second accept: status = %d, want 409", rec.Code)
	}
}

// TestStatusCodeMapping pins the error contract the frontend depends on.
func TestStatusCodeMapping(t *testing.T) {
	h := newTestServer(t)

	rec := do(h, "POST", "/api/rfqs", createRFQBody(1_000_000_00))
	rfq := decodeBody[domain.RFQ](t, rec)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"unknown rfq is 404", "GET", "/api/rfqs/nope", "", http.StatusNotFound},
		{"accept unknown rfq is 404", "POST", "/api/rfqs/nope/accept", "", http.StatusNotFound},
		{"quote on unknown rfq is 404", "POST", "/api/rfqs/nope/quotes",
			`{"dealerId":"dealer-a","rate":525,"haircut":200}`, http.StatusNotFound},
		{"accept with no quotes is 409", "POST", "/api/rfqs/" + rfq.ID + "/accept", "",
			http.StatusConflict},
		{"negative notional is 400", "POST", "/api/rfqs", createRFQBody(-5),
			http.StatusBadRequest},
		{"bad direction is 400", "POST", "/api/rfqs",
			`{"clientId":"client-a","direction":"SWAP","notional":100,"currency":"USD","collateral":{"cusip":"X"}}`,
			http.StatusBadRequest},
		{"missing dealer is 400", "POST", "/api/rfqs/" + rfq.ID + "/quotes",
			`{"dealerId":"","rate":525,"haircut":200}`, http.StatusBadRequest},
		{"implausible rate is 400", "POST", "/api/rfqs/" + rfq.ID + "/quotes",
			`{"dealerId":"dealer-a","rate":50000,"haircut":200}`, http.StatusBadRequest},
		{"malformed json is 400", "POST", "/api/rfqs", `{"clientId":`, http.StatusBadRequest},
		{"unknown field is 400", "POST", "/api/rfqs",
			`{"clientId":"client-a","bogus":1}`, http.StatusBadRequest},
		{"bad counterparty kind is 400", "POST", "/api/counterparties",
			`{"id":"x","name":"X","kind":"ROBOT"}`, http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, tc.method, tc.path, tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestCancelThenQuoteIsConflict(t *testing.T) {
	h := newTestServer(t)

	rec := do(h, "POST", "/api/rfqs", createRFQBody(1_000_000_00))
	rfq := decodeBody[domain.RFQ](t, rec)

	if rec := do(h, "POST", "/api/rfqs/"+rfq.ID+"/cancel", ""); rec.Code != http.StatusOK {
		t.Fatalf("cancel: status %d, body %s", rec.Code, rec.Body)
	}

	rec = do(h, "POST", "/api/rfqs/"+rfq.ID+"/quotes",
		`{"dealerId":"dealer-a","rate":525,"haircut":200}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("quote on cancelled rfq: status = %d, want 409", rec.Code)
	}
}

func TestListFilters(t *testing.T) {
	h := newTestServer(t)

	if rec := do(h, "GET", "/api/rfqs?state=OPEN&limit=5", ""); rec.Code != http.StatusOK {
		t.Errorf("list open: status = %d, want 200", rec.Code)
	}
	if rec := do(h, "GET", "/api/trades?limit=5", ""); rec.Code != http.StatusOK {
		t.Errorf("list trades: status = %d, want 200", rec.Code)
	}
	if rec := do(h, "GET", "/api/counterparties", ""); rec.Code != http.StatusOK {
		t.Errorf("list counterparties: status = %d, want 200", rec.Code)
	}
}

// --- Broker tests: no database required ---

func TestBrokerFanOut(t *testing.T) {
	b := NewBroker()
	a, c := b.subscribe(), b.subscribe()
	defer b.unsubscribe(a)
	defer b.unsubscribe(c)

	if got := b.Subscribers(); got != 2 {
		t.Fatalf("Subscribers() = %d, want 2", got)
	}

	b.Publish(Event{Type: "trade.executed", Payload: map[string]string{"id": "t1"}})

	for i, ch := range []chan Event{a, c} {
		select {
		case ev := <-ch:
			if ev.Type != "trade.executed" {
				t.Errorf("subscriber %d got %q, want trade.executed", i, ev.Type)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d received nothing", i)
		}
	}
}

// TestBrokerDoesNotBlockOnSlowSubscriber is the reason Publish uses a default
// case. A stalled browser tab must never be able to wedge the goroutine that
// just booked a trade.
func TestBrokerDoesNotBlockOnSlowSubscriber(t *testing.T) {
	b := NewBroker()
	slow := b.subscribe()
	defer b.unsubscribe(slow)

	done := make(chan struct{})
	go func() {
		// Far more events than the 16-slot buffer; none are ever read.
		for i := 0; i < 1000; i++ {
			b.Publish(Event{Type: "quote.submitted"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a subscriber that never reads")
	}
}

func TestBrokerConcurrentSubscribeAndPublish(t *testing.T) {
	b := NewBroker()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ch := b.subscribe()
			b.unsubscribe(ch)
		}()
		go func() {
			defer wg.Done()
			b.Publish(Event{Type: "rfq.created"})
		}()
	}
	wg.Wait()

	if got := b.Subscribers(); got != 0 {
		t.Errorf("Subscribers() = %d after cleanup, want 0", got)
	}
}

func TestSSEStreamEmitsEvents(t *testing.T) {
	b := NewBroker()

	srv := httptest.NewServer(http.HandlerFunc(b.Handle))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Wait for the handler to register before publishing, otherwise the event
	// races the subscription and the test flakes.
	for i := 0; i < 100 && b.Subscribers() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	b.Publish(Event{Type: "trade.executed", Payload: map[string]string{"id": "t1"}})

	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}

	got := string(buf[:n])
	if !bytes.Contains([]byte(got), []byte("event: trade.executed")) {
		t.Errorf("stream = %q, want an event: trade.executed frame", got)
	}
}
