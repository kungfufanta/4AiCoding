// v1_users.go — admin-managed users with roles (admin | operator | viewer).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

var (
	errBadUsername = errors.New("username must be 1-64 characters: letters, digits, . _ @ -")
	errBadPassword = errors.New("password must be at least 8 characters")
	errBadRole     = errors.New("role must be admin, operator or viewer")
	errUserExists  = errors.New("a user with that name already exists")
	errLastAdmin   = errors.New("this is the last enabled admin; create another admin first")
)

type v1User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toV1User(u *store.User) v1User {
	return v1User{ID: u.ID, Username: u.Username, Role: u.Role, Disabled: u.Disabled,
		CreatedAt: u.CreatedAt.Format(time.RFC3339), UpdatedAt: u.UpdatedAt.Format(time.RFC3339)}
}

func writeUserErr(w http.ResponseWriter, err error) {
	switch err {
	case errBadUsername, errBadPassword, errBadRole:
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errUserExists, errLastAdmin:
		writeV1Err(w, http.StatusConflict, "conflict", err.Error())
	case store.ErrNotFound:
		writeV1Err(w, http.StatusNotFound, "not_found", "no such user")
	default:
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) createUser(r *http.Request, username, password, role string) (*store.User, error) {
	return createUser(r.Context(), s.Store, username, password, role)
}

func createUser(ctx context.Context, st *store.Store, username, password, role string) (*store.User, error) {
	if !usernameRE.MatchString(username) {
		return nil, errBadUsername
	}
	if len(password) < minPasswordLen {
		return nil, errBadPassword
	}
	if !auth.ValidRole(role) {
		return nil, errBadRole
	}
	hash, err := console.HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &store.User{ID: newULID(), Username: username, PasswordHash: hash, Role: role}
	if err := st.CreateUser(ctx, u); err != nil {
		if err == store.ErrConflict {
			return nil, errUserExists
		}
		return nil, err
	}
	return u, nil
}

// BootstrapAdmin creates the first admin from SANDBOXD_BOOTSTRAP_ADMIN
// ("username:password") when no user exists yet. It never touches an
// install that already has users, so the env can stay set.
func BootstrapAdmin(ctx context.Context, st *store.Store, spec string) (created bool, username string, err error) {
	if spec == "" {
		return false, "", nil
	}
	n, err := st.CountUsers(ctx)
	if err != nil || n > 0 {
		return false, "", err
	}
	user, pass, ok := strings.Cut(spec, ":")
	if !ok {
		return false, "", errors.New(`SANDBOXD_BOOTSTRAP_ADMIN must be "username:password"`)
	}
	u, err := createUser(ctx, st, strings.TrimSpace(user), pass, auth.RoleAdmin)
	if err != nil {
		return false, "", err
	}
	return true, u.Username, nil
}

// GET /v1/users
func (s *Server) v1ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]v1User, 0, len(users))
	for _, u := range users {
		out = append(out, toV1User(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// POST /v1/users {username, password, role}
func (s *Server) v1CreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	if body.Role == "" {
		body.Role = auth.RoleOperator
	}
	u, err := s.createUser(r, strings.TrimSpace(body.Username), body.Password, body.Role)
	if err != nil {
		writeUserErr(w, err)
		return
	}
	s.auditAction(r, audit.Entry{Action: "user.created", Target: u.ID,
		Detail: map[string]any{"username": u.Username, "role": u.Role}})
	writeJSON(w, http.StatusCreated, toV1User(u))
}

// PATCH /v1/users/{id} {role?, disabled?, password?}
func (s *Server) v1UpdateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	u, err := s.Store.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		writeUserErr(w, err)
		return
	}
	wasActiveAdmin := u.Role == auth.RoleAdmin && !u.Disabled
	changes := map[string]any{}
	if body.Role != nil {
		if !auth.ValidRole(*body.Role) {
			writeUserErr(w, errBadRole)
			return
		}
		u.Role = *body.Role
		changes["role"] = u.Role
	}
	if body.Disabled != nil {
		u.Disabled = *body.Disabled
		changes["disabled"] = u.Disabled
	}
	if body.Password != nil {
		if len(*body.Password) < minPasswordLen {
			writeUserErr(w, errBadPassword)
			return
		}
		h, err := console.HashPassword(*body.Password)
		if err != nil {
			writeUserErr(w, err)
			return
		}
		u.PasswordHash = h
		changes["password"] = "reset"
	}
	if wasActiveAdmin && (u.Role != auth.RoleAdmin || u.Disabled) {
		if n, err := s.Store.CountActiveAdmins(r.Context()); err != nil || n <= 1 {
			writeUserErr(w, errLastAdmin)
			return
		}
	}
	if err := s.Store.UpdateUser(r.Context(), u); err != nil {
		writeUserErr(w, err)
		return
	}
	if u.Disabled || body.Password != nil || body.Role != nil {
		_ = s.Store.DeleteUserSessions(r.Context(), u.ID) // re-auth with the new rights
	}
	s.auditAction(r, audit.Entry{Action: "user.updated", Target: u.ID, Detail: changes})
	writeJSON(w, http.StatusOK, toV1User(u))
}

// DELETE /v1/users/{id}
func (s *Server) v1DeleteUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.Store.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		writeUserErr(w, err)
		return
	}
	if u.Role == auth.RoleAdmin && !u.Disabled {
		if n, err := s.Store.CountActiveAdmins(r.Context()); err != nil || n <= 1 {
			writeUserErr(w, errLastAdmin)
			return
		}
	}
	if err := s.Store.DeleteUser(r.Context(), u.ID); err != nil {
		writeUserErr(w, err)
		return
	}
	s.auditAction(r, audit.Entry{Action: "user.deleted", Target: u.ID, Detail: map[string]any{"username": u.Username}})
	w.WriteHeader(http.StatusNoContent)
}

// --- audit ----------------------------------------------------------

// GET /v1/audit?before=&limit=&action=&target=
func (s *Server) v1ListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := atoiRange(v, 1, 1000)
		if err != nil {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "limit must be 1..1000")
			return
		}
		limit = n
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := atoiRange(v, 1, 1<<62)
		if err != nil {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "before must be a positive id")
			return
		}
		before = int64(n)
	}
	rows, err := s.Store.ListAudit(r.Context(), before, limit, q.Get("action"), q.Get("target"))
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if rows == nil {
		rows = []store.AuditRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows})
}

// GET /v1/audit/verify — walk the hash chain.
func (s *Server) v1VerifyAudit(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.VerifyAudit(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// atoiRange parses a decimal integer within [min, max].
func atoiRange(v string, min, max int) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, errors.New("out of range")
	}
	return n, nil
}
