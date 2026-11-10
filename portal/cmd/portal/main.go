// portal — the SaaS layer in front of sandboxd (docs/PORTAL_PLAN.md).
//
// S0 skeleton: read config from the environment, open Postgres, apply
// the embedded migrations and serve /healthz + /readyz. Later
// milestones mount the BFF (dashboard) and the public /v1 API on the
// same server.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/config"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/httpapi"
	"github.com/tastyeffectco/sandboxd/portal/internal/logging"
	"github.com/tastyeffectco/sandboxd/portal/internal/webfs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "portal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	log := logging.NewLogger(os.Getenv("PORTAL_DEBUG") != "")
	cfg, err := config.Parse(os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, log, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	srv := &httpapi.Server{Log: log, Pool: pool}
	mux := srv.Handler()
	// The SPA (embedded via internal/webfs) rides on the same origin:
	// dashboard and BFF share cookies (PORTAL_PLAN 3.1).
	mux.Handle("/", webfs.Handler())
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           logging.Middleware(log, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("portal listening", "addr", cfg.Addr, "public_url", cfg.PublicURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}
