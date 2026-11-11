package api

import (
	"context"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// storeResolver implements auth.CredentialResolver over the SQLite store.
// Wired into the auth middleware in main.go.
type storeResolver struct{ st *store.Store }

// NewStoreResolver returns an auth.CredentialResolver backed by st.
func NewStoreResolver(st *store.Store) auth.CredentialResolver { return storeResolver{st: st} }

func (r storeResolver) ResolveSession(ctx context.Context, cookieValue string) (auth.Principal, bool) {
	if r.st == nil || cookieValue == "" {
		return auth.Principal{}, false
	}
	h := console.HashToken(cookieValue)
	si, found, err := r.st.LookupSession(ctx, h)
	if err != nil || !found || time.Now().Unix() > si.Expires {
		return auth.Principal{}, false
	}
	_ = r.st.TouchSession(ctx, h, time.Now().Unix())
	return auth.Principal{Owner: si.Owner, User: si.User.Username, Role: si.User.Role}, true
}

func (r storeResolver) ResolveAPIKey(ctx context.Context, presented string) (auth.Principal, bool) {
	if r.st == nil || presented == "" {
		return auth.Principal{}, false
	}
	k, found, err := r.st.LookupAPIKey(ctx, console.HashToken(presented))
	if err != nil || !found {
		return auth.Principal{}, false
	}
	if k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
		return auth.Principal{}, false
	}
	_ = r.st.TouchAPIKey(ctx, k.ID, time.Now().Unix())
	return auth.Principal{Owner: store.DefaultTenant, User: k.Name, Role: k.Role,
		Scopes: k.Scopes, SandboxID: k.SandboxID, RequireExternal: k.RequireExternal}, true
}
