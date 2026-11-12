// mcp-server is the 4AiCoding Portal MCP server (PORTAL_PLAN_V2 P6).
// It exposes the Portal's sandbox surface as MCP tools an agent can
// call without writing HTTP. Run as a standalone binary or as a
// subcommand of the Portal binary (cmd/portal mcp-server).
//
// Usage:
//
//	PORTAL_URL=https://chat.devhub.io.vn \
//	MCP_BIND=:9100 \
//	mcp-server
//
// The server forwards every tool call to PORTAL_URL/v1/... with the
// caller's Bearer token (read from the MCP request's Authorization
// header). The Portal's scope gate applies: a key without
// sandboxes:files gets 403 on read_file/write_file/list_files.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/mcp"
)

func main() {
	addr := flag.String("addr", envOr("MCP_BIND", ":9100"), "bind address")
	portalURL := flag.String("portal-url", envOr("PORTAL_URL", "http://127.0.0.1:8080"),
		"Portal /v1 base URL")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	srv := &mcp.Server{
		PortalURL: *portalURL,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: http.HandlerFunc(srv.Handle),
	}
	go func() {
		log.Info("mcp-server listening", "addr", *addr, "portal_url", *portalURL)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen failed", "error", err.Error())
			os.Exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
