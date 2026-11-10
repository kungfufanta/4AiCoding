package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func memStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// fileStore opens a real file-backed database (the in-memory memStore gives each
// connection its OWN empty DB, which breaks genuinely-concurrent reads).
func fileStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db") + "?_fk=1"
	st, err := store.Open(context.Background(), dsn, "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// newSandboxRowServer returns a Server backed by a file store holding one
// running sandbox row.
func newSandboxRowServer(t *testing.T) (*Server, string) {
	t.Helper()
	st := fileStore(t)
	t.Cleanup(func() { st.Close() })
	lm := loopback.New()
	lm.Root = t.TempDir()
	s := &Server{Store: st, Loopback: lm, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := newULID()
	if err := st.Create(context.Background(), &store.Sandbox{ID: id, Status: "running", ExternalUserID: nullStr("local")}); err != nil {
		t.Fatal(err)
	}
	return s, id
}

// reqAs builds a request carrying an actor (tenant) in context.
func reqAs(method, path, body, actor string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(auth.WithActor(r.Context(), auth.Actor{Kind: "service", Name: actor}))
	return r
}
