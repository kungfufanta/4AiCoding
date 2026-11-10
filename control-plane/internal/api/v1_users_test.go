package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func TestUsersRolesAndLastAdmin(t *testing.T) {
	h, s := authTestServer(t)
	w := doAuth(t, h, "POST", "/v1/auth/setup", `{"username":"root","password":"correcthorse"}`, "", "")
	if w.Code != 204 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	admin := sessionFrom(t, w)
	if w := doAuth(t, h, "GET", "/v1/auth/me", "", admin, ""); !strings.Contains(w.Body.String(), `"role":"admin"`) ||
		!strings.Contains(w.Body.String(), `"user":"root"`) {
		t.Fatalf("me: %s", w.Body)
	}
	// Create an operator and a viewer.
	w = doAuth(t, h, "POST", "/v1/users", `{"username":"op","password":"operator-pw","role":"operator"}`, admin, "")
	if w.Code != 201 {
		t.Fatalf("create op: %d %s", w.Code, w.Body)
	}
	var op v1User
	_ = json.Unmarshal(w.Body.Bytes(), &op)
	if w := doAuth(t, h, "POST", "/v1/users", `{"username":"op","password":"x-long-pw","role":"viewer"}`, admin, ""); w.Code != 409 {
		t.Fatalf("duplicate username: %d", w.Code)
	}
	if w := doAuth(t, h, "POST", "/v1/users", `{"username":"v","password":"viewer-pw","role":"root"}`, admin, ""); w.Code != 400 {
		t.Fatalf("bad role: %d", w.Code)
	}
	// Operator signs in; cannot manage users or read the audit log.
	w = doAuth(t, h, "POST", "/v1/auth/login", `{"username":"op","password":"operator-pw"}`, "", "")
	if w.Code != 204 {
		t.Fatalf("op login: %d %s", w.Code, w.Body)
	}
	opCk := sessionFrom(t, w)
	if w := doAuth(t, h, "GET", "/v1/users", "", opCk, ""); w.Code != 403 {
		t.Fatalf("operator listing users: %d", w.Code)
	}
	if w := doAuth(t, h, "GET", "/v1/audit", "", opCk, ""); w.Code != 403 {
		t.Fatalf("operator reading audit: %d", w.Code)
	}
	if w := doAuth(t, h, "POST", "/v1/auth/login", `{"username":"op","password":"wrong-pass"}`, "", ""); w.Code != 401 {
		t.Fatalf("bad password: %d", w.Code)
	}
	// Disabling the operator kills its session.
	if w := doAuth(t, h, "PATCH", "/v1/users/"+op.ID, `{"disabled":true}`, admin, ""); w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body)
	}
	if w := doAuth(t, h, "GET", "/v1/sandboxes", "", opCk, ""); w.Code != 401 {
		t.Fatalf("disabled user's session still valid: %d", w.Code)
	}
	// The last admin cannot be demoted or deleted.
	users, _ := s.Store.ListUsers(context.Background())
	var rootID string
	for _, u := range users {
		if u.Username == "root" {
			rootID = u.ID
		}
	}
	if w := doAuth(t, h, "PATCH", "/v1/users/"+rootID, `{"role":"viewer"}`, admin, ""); w.Code != 409 {
		t.Fatalf("demote last admin: %d", w.Code)
	}
	if w := doAuth(t, h, "DELETE", "/v1/users/"+rootID, "", admin, ""); w.Code != 409 {
		t.Fatalf("delete last admin: %d", w.Code)
	}
	// Audit chain covers these actions and verifies.
	w = doAuth(t, h, "GET", "/v1/audit/verify", "", admin, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("verify: %d %s", w.Code, w.Body)
	}
}

func TestAPIKeyLeastPrivilege(t *testing.T) {
	h, s := authTestServer(t)
	w := doAuth(t, h, "POST", "/v1/auth/setup", `{"password":"correcthorse"}`, "", "")
	admin := sessionFrom(t, w)
	_ = s.Store.Create(context.Background(), &store.Sandbox{ID: "01JZZZZZZZZZZZZZZZZZZZZZZA", Status: "stopped"})
	mint := func(body string) (int, map[string]any) {
		w := doAuth(t, h, "POST", "/v1/api-keys", body, admin, "")
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, k := mint(`{"name":"viewer","role":"viewer"}`)
	if code != 201 || k["role"] != "viewer" {
		t.Fatalf("viewer key: %d %v", code, k)
	}
	viewerKey := k["key"].(string)
	if w := doAuth(t, h, "GET", "/v1/sandboxes", "", "", viewerKey); w.Code != 200 {
		t.Fatalf("viewer key read: %d", w.Code)
	}
	if w := doAuth(t, h, "POST", "/v1/sandboxes", `{}`, "", viewerKey); w.Code != 403 {
		t.Fatalf("viewer key write: %d", w.Code)
	}
	code, k = mint(`{"name":"bound","sandbox_id":"01JZZZZZZZZZZZZZZZZZZZZZZA"}`)
	if code != 201 {
		t.Fatalf("bound key: %d %v", code, k)
	}
	bound := k["key"].(string)
	if w := doAuth(t, h, "GET", "/v1/sandboxes/01JZZZZZZZZZZZZZZZZZZZZZZA", "", "", bound); w.Code != 200 {
		t.Fatalf("bound key own sandbox: %d %s", w.Code, w.Body)
	}
	if w := doAuth(t, h, "GET", "/v1/sandboxes", "", "", bound); w.Code != 403 {
		t.Fatalf("bound key list: %d", w.Code)
	}
	if code, _ := mint(`{"name":"ghost","sandbox_id":"nope"}`); code != 400 {
		t.Fatalf("binding to a missing sandbox: %d", code)
	}
	code, k = mint(`{"name":"short","ttl_seconds":3600}`)
	if code != 201 || k["expires_at"] == nil {
		t.Fatalf("ttl key: %d %v", code, k)
	}
	// An expired key stops working.
	code, k = mint(`{"name":"expired","ttl_seconds":1}`)
	if code != 201 {
		t.Fatal(code)
	}
	if _, err := s.Store.DB().ExecContext(context.Background(), `UPDATE api_key SET expires_at = 1 WHERE name = 'expired'`); err != nil {
		t.Fatal(err)
	}
	if w := doAuth(t, h, "GET", "/v1/sandboxes", "", "", k["key"].(string)); w.Code != 401 {
		t.Fatalf("expired key: %d", w.Code)
	}
	if code, _ := mint(`{"name":"neg","ttl_seconds":-5}`); code != 400 {
		t.Fatalf("negative ttl: %d", code)
	}
}
