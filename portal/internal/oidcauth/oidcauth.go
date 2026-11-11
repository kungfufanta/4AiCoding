// Package oidcauth wraps the OIDC relying-party side of the Portal's
// login (PORTAL_PLAN 3.1): Authorization Code + PKCE against any
// standards-compliant provider (Zitadel by default, swappable for
// Keycloak/Auth0/Clerk). The Portal never stores passwords — SSO
// only.
package oidcauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Claims are the ID-token fields the Portal relies on.
type Claims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name"`
	// EmailVerified is nil when the provider omits the claim.
	EmailVerified *bool `json:"email_verified"`
}

// Provider bundles the discovery result and the OAuth2 config.
type Provider struct {
	oauth     *oauth2.Config
	verifier  *oidc.IDTokenVerifier
	clientID  string
	authStyle oauth2.AuthStyle
}

// New performs discovery on issuer and wires the code flow.
func New(ctx context.Context, issuer, clientID, clientSecret, redirectURL string) (*Provider, error) {
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery %s: %w", issuer, err)
	}
	return &Provider{
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     p.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: p.Verifier(&oidc.Config{ClientID: clientID}),
		clientID: clientID,
	}, nil
}

// RandomToken mints a url-safe random value (state, verifier, nonce).
func RandomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("oidcauth: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// PKCEPair derives the S256 code challenge for a verifier.
func PKCEPair() (verifier, challenge string) {
	verifier = RandomToken()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthCodeURL builds the provider redirect with state, PKCE and
// nonce.
func (p *Provider) AuthCodeURL(state, codeChallenge, nonce string) string {
	return p.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oidc.Nonce(nonce),
	)
}

// ErrStateMismatch marks a bad state/nonce (possible CSRF on the
// callback).
var ErrStateMismatch = errors.New("oidc state or nonce mismatch")

// Exchange swaps the code for tokens, verifies the ID token
// (signature, audience, nonce) and returns the claims.
func (p *Provider) Exchange(ctx context.Context, code, codeVerifier, nonce string) (Claims, error) {
	tok, err := p.oauth.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", codeVerifier))
	if err != nil {
		return Claims{}, fmt.Errorf("oidc code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Claims{}, errors.New("oidc token response has no id_token")
	}
	idTok, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return Claims{}, fmt.Errorf("verify id_token: %w", err)
	}
	if nonce != "" && idTok.Nonce != nonce {
		return Claims{}, ErrStateMismatch
	}
	var c Claims
	if err := idTok.Claims(&c); err != nil {
		return Claims{}, fmt.Errorf("decode id_token claims: %w", err)
	}
	if c.Sub == "" {
		return Claims{}, errors.New("id_token has no sub claim")
	}
	return c, nil
}
