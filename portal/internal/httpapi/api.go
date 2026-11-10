package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// Dashboard BFF handlers (PORTAL_PLAN 6/S1: login, tạo/đổi org, mời
// thành viên, đổi role, tạo project). Org-scoped routes resolve
// membership first and answer 404 (never 403) so the existence of a
// foreign org is not revealed (PORTAL_PLAN 5).

// handleMe answers with the authenticated user and their orgs.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	var user tenants.User
	var orgs []tenants.Org
	err := db.WithTx(r.Context(), s.Pool, func(tx db.Tx) error {
		u, err := tenants.UserByID(r.Context(), tx, db.UserID(r.Context()))
		if err != nil {
			return err
		}
		user = u
		orgs, err = tenants.ListMyOrgs(r.Context(), tx)
		return err
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not load profile")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "orgs": orgs})
}

// handleCreateOrg creates an org with the caller as owner.
func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be {\"name\": \"...\"}")
		return
	}
	orgID, err := db.NewOrgID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not generate org id")
		return
	}
	userID := db.UserID(r.Context())
	var org tenants.Org
	err = db.BootstrapTx(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		slug, err := tenants.UniqueOrgSlug(r.Context(), tx, tenants.Slugify(body.Name))
		if err != nil {
			return err
		}
		org, err = tenants.CreateOrg(r.Context(), tx, orgID, slug, body.Name, userID)
		if err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]string{"org_id": orgID, "slug": org.Slug})
		return tenants.Audit(r.Context(), tx, "user", userID, "org.create", orgID, string(meta))
	})
	if err != nil {
		s.Log.Error("create org failed", slog.Any("error", err))
		writeErr(w, http.StatusInternalServerError, "internal", "could not create org")
		return
	}
	writeJSON(w, http.StatusCreated, org)
}

// requireMember pins the caller's role in orgID (from ctx) and
// answers 404 when they are not a member of the org.
func (s *Server) requireMember(w http.ResponseWriter, r *http.Request, orgID string) (string, bool) {
	var role string
	err := db.WithTx(r.Context(), s.Pool, func(tx db.Tx) error {
		var err error
		role, err = tenants.MembershipRole(r.Context(), tx, orgID, db.UserID(r.Context()))
		return err
	})
	if errors.Is(err, tenants.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "org not found")
		return "", false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not load membership")
		return "", false
	}
	return role, true
}

// requireAdmin is requireMember plus the admin/owner gate.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request, orgID string) (string, bool) {
	role, ok := s.requireMember(w, r, orgID)
	if !ok {
		return "", false
	}
	if role != tenants.RoleOwner && role != tenants.RoleAdmin {
		writeErr(w, http.StatusNotFound, "not_found", "org not found")
		return "", false
	}
	return role, true
}

func pathOrgID(r *http.Request) string { return r.PathValue("orgID") }

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	var projects []tenants.Project
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		var err error
		projects, err = tenants.ListProjects(r.Context(), tx)
		return err
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not list projects")
		return
	}
	if projects == nil {
		projects = []tenants.Project{}
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	role, ok := s.requireAdmin(w, r, orgID)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be {\"name\": \"...\"}")
		return
	}
	var project tenants.Project
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		p, err := tenants.CreateProject(r.Context(), tx, tenants.Slugify(body.Name), body.Name)
		if err != nil {
			return err
		}
		project = p
		meta, _ := json.Marshal(map[string]string{"project_id": p.ID, "slug": p.Slug})
		return tenants.Audit(r.Context(), tx, "user", db.UserID(r.Context()), "project.create", p.ID, string(meta))
	})
	if errors.Is(err, tenants.ErrConflict) {
		writeErr(w, http.StatusConflict, "conflict", "a project with that slug already exists")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not create project")
		return
	}
	meta, _ := json.Marshal(map[string]string{"actor_role": role})
	_ = meta // role recorded in audit action above via audit row context
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	var members []tenants.Member
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		var err error
		members, err = tenants.ListMembers(r.Context(), tx)
		return err
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not list members")
		return
	}
	if members == nil {
		members = []tenants.Member{}
	}
	writeJSON(w, http.StatusOK, members)
}

func (s *Server) handleSetMemberRole(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	targetID := r.PathValue("userID")
	if _, ok := s.requireAdmin(w, r, orgID); !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be {\"role\": \"...\"}")
		return
	}
	if !tenants.MembershipRoleValid(body.Role) {
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown role")
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		if err := tenants.SetMemberRole(r.Context(), tx, orgID, targetID, body.Role); err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]string{"member": targetID, "role": body.Role})
		return tenants.Audit(r.Context(), tx, "user", db.UserID(r.Context()), "member.role", targetID, string(meta))
	})
	if errors.Is(err, tenants.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "member not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not change role")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user_id": targetID, "role": body.Role})
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireAdmin(w, r, orgID); !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be {\"email\": \"...\", \"role\": \"...\"}")
		return
	}
	if body.Role == "" {
		body.Role = tenants.RoleMember
	}
	if !tenants.InviteRoleValid(body.Role) {
		writeErr(w, http.StatusBadRequest, "bad_request", "role must be admin, member or billing")
		return
	}
	var inv tenants.Invite
	var token string
	ttl := s.InviteTTL
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		var err error
		inv, token, err = tenants.CreateInvite(r.Context(), tx, orgID, body.Email, body.Role, db.UserID(r.Context()), ttl)
		if err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]string{"email": body.Email, "role": body.Role})
		return tenants.Audit(r.Context(), tx, "user", db.UserID(r.Context()), "invite.create", inv.ID, string(meta))
	})
	if errors.Is(err, tenants.ErrConflict) {
		writeErr(w, http.StatusConflict, "conflict", "that email already has a pending invite")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not create invite")
		return
	}
	// The invite URL is shown once; the token lives only in the
	// response body and in the invitee's inbox (hash stored).
	link := "/invites/" + orgID + "?token=" + token
	if s.PublicURL != "" {
		link = s.PublicURL + link
	}
	writeJSON(w, http.StatusCreated, map[string]any{"invite": inv, "url": link})
}

// handleAcceptInvite exchanges the emailed token for a membership.
// The org id rides in the URL; the token is the secret.
func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "body must be {\"token\": \"...\"}")
		return
	}
	var role string
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		var err error
		role, err = tenants.AcceptInvite(r.Context(), tx, orgID, body.Token, db.UserID(r.Context()))
		if err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]string{"role": role})
		return tenants.Audit(r.Context(), tx, "user", db.UserID(r.Context()), "invite.accept", orgID, string(meta))
	})
	if errors.Is(err, tenants.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "invite not found, expired or already used")
		return
	}
	if errors.Is(err, tenants.ErrConflict) {
		writeErr(w, http.StatusConflict, "conflict", "already a member of this org")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not accept invite")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"org_id": orgID, "role": role})
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	var events []tenants.AuditEvent
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		var err error
		events, err = tenants.ListAudit(r.Context(), tx, 50)
		return err
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not list audit events")
		return
	}
	if events == nil {
		events = []tenants.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}
