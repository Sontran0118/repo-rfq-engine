// Command server runs the repo RFQ lifecycle service.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sontran0118/repo-rfq-engine/internal/api"
	"github.com/Sontran0118/repo-rfq-engine/internal/store"
)

const defaultDSN = "rfq:rfq@tcp(127.0.0.1:3306)/rfq_engine?parseTime=true&loc=UTC"

func main() {
	addr := flag.String("addr", envOr("RFQ_ADDR", ":8080"), "listen address")
	dsn := flag.String("dsn", envOr("RFQ_DSN", defaultDSN), "MySQL DSN")
	seed := flag.Bool("seed", false, "insert demo counterparties on start")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, *dsn)
	if err != nil {
		log.Error("cannot reach mysql", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		log.Error("migration failed", "error", err)
		os.Exit(1)
	}
	if *seed {
		if err := seedDemo(ctx, st); err != nil {
			log.Error("seed failed", "error", err)
			os.Exit(1)
		}
		log.Info("seeded demo counterparties")
	}

	srv := api.New(st, log)
	srv.StartExpiryWorker(ctx, 10*time.Second)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: the SSE stream is deliberately long-lived, and a write
		// deadline would sever the blotter mid-session.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown failed", "error", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// seedDemo inserts the counterparties the UI needs to be usable out of the box.
func seedDemo(ctx context.Context, st *store.Store) error {
	demo := []store.Counterparty{
		{ID: "client-pension-a", Name: "Meridian Pension Fund", Kind: "CLIENT"},
		{ID: "client-mmf-b", Name: "Harbor Money Market Fund", Kind: "CLIENT"},
		{ID: "dealer-gs", Name: "Dealer Alpha", Kind: "DEALER"},
		{ID: "dealer-ms", Name: "Dealer Bravo", Kind: "DEALER"},
		{ID: "dealer-cs", Name: "Dealer Charlie", Kind: "DEALER"},
	}
	for _, c := range demo {
		if err := st.UpsertCounterparty(ctx, c); err != nil {
			return err
		}
	}
	return nil
}
