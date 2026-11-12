// portal — the SaaS layer in front of sandboxd (docs/PORTAL_PLAN.md).
//
// S0 skeleton: read config from the environment, open Postgres, apply
// the embedded migrations and serve /healthz + /readyz. Later
// milestones mount the BFF (dashboard) and the public /v1 API on the
// same server.
//
// A3.1 adds the agent workers: N goroutines (PORTAL_AGENT_WORKERS,
// default 2, 0 = off) claim runs from Postgres and execute them in
// this process. `portal agent-worker` runs the same workers without
// the HTTP server (the seam A6 needs to move them to a Deployment).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent"
	"github.com/tastyeffectco/sandboxd/portal/internal/agenttoken"
	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/config"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/httpapi"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/logging"
	"github.com/tastyeffectco/sandboxd/portal/internal/oidcauth"
	"github.com/tastyeffectco/sandboxd/portal/internal/passwordauth"
	"github.com/tastyeffectco/sandboxd/portal/internal/ratelimit"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
	"github.com/tastyeffectco/sandboxd/portal/internal/session"
	"github.com/tastyeffectco/sandboxd/portal/internal/webfs"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "agent-worker" {
		if err := runWorkerOnly(); err != nil {
			fmt.Fprintf(os.Stderr, "portal agent-worker: %v\n", err)
			os.Exit(1)
		}
		return
	}
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

	// Migrations run as the (owner) login user; serving runs as the
	// non-owner app role so Row Level Security applies. Two pools,
	// because SET ROLE sticks per connection.
	adminPool, err := db.Open(ctx, log, cfg.DatabaseURL, "")
	if err != nil {
		return err
	}
	if err := db.Migrate(ctx, adminPool, log); err != nil {
		adminPool.Close()
		return fmt.Errorf("migrate: %w", err)
	}
	// Bootstrap password login (owner decision 2026-11-11): the
	// local admin is ensured with the configured password on every
	// boot, so PORTAL_ADMIN_PASSWORD rotation is just a redeploy.
	var bootstrapAdminID string
	if cfg.PasswordLogin {
		id, err := passwordauth.EnsureBootstrap(ctx, adminPool, log,
			cfg.AdminUsername, cfg.AdminPassword, cfg.AdminEmail)
		if err != nil {
			adminPool.Close()
			return fmt.Errorf("bootstrap password login: %w", err)
		}
		bootstrapAdminID = id
	} else if err := passwordauth.DisableAll(ctx, adminPool, log); err != nil {
		adminPool.Close()
		return fmt.Errorf("disable password login: %w", err)
	}
	adminPool.Close()

	pool, err := db.Open(ctx, log, cfg.DatabaseURL, cfg.DBRole)
	if err != nil {
		return err
	}
	defer pool.Close()

	// OIDC login (plan 3.1): enabled only when issuer + client id are
	// set. The callback URL is <public>/auth/callback, so SSO needs a
	// public URL too.
	var provider *oidcauth.Provider
	if cfg.OIDCIssuer != "" && cfg.OIDCClientID != "" {
		if cfg.PublicURL == "" {
			log.Warn("PORTAL_OIDC_* set but PORTAL_PUBLIC_URL missing — login disabled")
		} else {
			pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
			provider, err = oidcauth.New(pctx, cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret,
				cfg.PublicURL+"/auth/callback")
			pcancel()
			if err != nil {
				return fmt.Errorf("oidc: %w", err)
			}
			log.Info("oidc login enabled", "issuer", cfg.OIDCIssuer, "client_id", cfg.OIDCClientID)
		}
	}

	// Agent tokens (PORTAL_PLAN 6/S2): the EdDSA key pair lives in
	// this process; tokens die with it, API keys do not.
	signer, err := agenttoken.NewSigner()
	if err != nil {
		return fmt.Errorf("agent token signer: %w", err)
	}
	keys := &apikeys.Verifier{Pool: pool, Log: log}

	var chat *llm.Client
	if cfg.ChatEnabled() {
		chat = &llm.Client{BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel, Flavor: cfg.LLMFlavor}
		log.Info("chat enabled", "llm_base_url", cfg.LLMBaseURL, "model", cfg.LLMModel, "flavor", cfg.LLMFlavor)
	} else {
		log.Info("chat disabled: PORTAL_DEFAULT_LLM_API_KEY not set")
	}

	// Sandboxd gateway (PORTAL_PLAN 6/S3): nil client disables the
	// whole sandbox surface (dev stacks without a backend).
	var sandboxdClient *sandboxd.Client
	if cfg.SandboxdURL != "" {
		sandboxdClient = sandboxd.New(cfg.SandboxdURL, cfg.SandboxdAPIKey, cfg.SandboxdTimeout)
		log.Info("sandboxd gateway enabled", "url", cfg.SandboxdURL,
			"api_key", cfg.SandboxdAPIKey != "",
			"webhook", cfg.SandboxdWebhookSecret != "")
	} else {
		log.Info("sandboxd gateway disabled (PORTAL_SANDBOXD_URL unset)")
	}

	// A3.1: the agent stack. Workers run in this process (decision
	// §2-4); `0` disables them (another process — e.g. the dedicated
	// `portal agent-worker` pod — owns the queue). The notifier is
	// shared with the HTTP side so SSE followers wake instantly.
	var workers []*agent.Worker
	var notifier *agent.Notifier
	if cfg.AgentWorkers > 0 {
		notifier = agent.NewNotifier(ctx, log, cfg.DatabaseURL)
		// A3.2: the workers execute tools through the same sandboxd
		// gateway the HTTP surface uses; without it every run answers
		// sandbox_unavailable (the agent cannot work honestly without a
		// sandbox backend).
		workers = agent.StartPool(ctx, log, pool, notifier,
			agent.ConfigSource{Pool: pool, Fallback: chat}, sandboxdClient, cfg.AgentWorkers)
		log.Info("agent workers enabled", "workers", cfg.AgentWorkers, "sandboxd", sandboxdClient != nil)
	} else {
		log.Info("agent workers disabled (PORTAL_AGENT_WORKERS=0)")
	}
	defer func() {
		for _, w := range workers {
			w.Wait()
		}
	}()

	srv := &httpapi.Server{
		Log:           log,
		Pool:          pool,
		Sessions:      session.New(pool, cfg.SessionTTL),
		OIDC:          provider,
		PasswordLogin: cfg.PasswordLogin,
		Keys:          keys,
		Tokens:        signer,
		LLM:           chat,
		Sandboxd:      sandboxdClient,
		WebhookSecret: cfg.SandboxdWebhookSecret,
		PublicURL:     cfg.PublicURL,
		SecureCookies: strings.HasPrefix(cfg.PublicURL, "https://"),
		SessionTTL:    cfg.SessionTTL,
		// P5: quota + rate limit. QuotaEnabled stays true in
		// production; tests toggle it off to isolate behaviour from
		// the plan catalogue.
		QuotaEnabled: true,
		Limiter:      ratelimit.New(),
		// A2: platform admin = the bootstrap admin (password login).
		// /api/admin/llm/* routes are enabled iff a bootstrap admin
		// was created (PORTAL_ADMIN_PASSWORD set).
		PlatformAdminEnabled: cfg.PasswordLogin,
		BootstrapAdminID:     bootstrapAdminID,
		AgentNotify:          notifier,
	}
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
		// Workers first: they get the shutdown grace (cancel active
		// runs as worker_shutdown), then the HTTP server drains.
		for _, w := range workers {
			w.Shutdown(agent.DefaultShutdownGrace)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// runWorkerOnly — `portal agent-worker`: migrations + workers, no
// HTTP listener. Same config, same queue; A6 turns this into the
// dedicated worker Deployment.
func runWorkerOnly() error {
	log := logging.NewLogger(os.Getenv("PORTAL_DEBUG") != "")
	cfg, err := config.Parse(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	adminPool, err := db.Open(ctx, log, cfg.DatabaseURL, "")
	if err != nil {
		return err
	}
	if err := db.Migrate(ctx, adminPool, log); err != nil {
		adminPool.Close()
		return fmt.Errorf("migrate: %w", err)
	}
	adminPool.Close()

	pool, err := db.Open(ctx, log, cfg.DatabaseURL, cfg.DBRole)
	if err != nil {
		return err
	}
	defer pool.Close()

	n := cfg.AgentWorkers
	if n == 0 {
		n = 2 // the dedicated process defaults to two workers
	}
	var sandboxdClient *sandboxd.Client
	if cfg.SandboxdURL != "" {
		sandboxdClient = sandboxd.New(cfg.SandboxdURL, cfg.SandboxdAPIKey, cfg.SandboxdTimeout)
	}
	notifier := agent.NewNotifier(ctx, log, cfg.DatabaseURL)
	workers := agent.StartPool(ctx, log, pool, notifier,
		agent.ConfigSource{Pool: pool}, sandboxdClient, n)
	log.Info("agent worker process started", "workers", n, "sandboxd", sandboxdClient != nil)

	<-ctx.Done()
	for _, w := range workers {
		w.Shutdown(agent.DefaultShutdownGrace)
	}
	for _, w := range workers {
		w.Wait()
	}
	return nil
}
