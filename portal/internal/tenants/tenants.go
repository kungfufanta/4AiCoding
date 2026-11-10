// Package tenants holds the repository queries for the tenant model
// (PORTAL_PLAN 3.4): users, orgs, memberships, invites, projects and
// audit events. Every function takes a pgx.Tx so callers run inside
// one of the scoped transactions from internal/db — the queries are
// allowed to "forget" WHERE org_id because Row Level Security does
// the final check (that property is asserted by rls_test.go).
package tenants

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Role names inside an org (PORTAL_PLAN 3.1). These are Portal roles
// and are unrelated to sandboxd's admin/operator/viewer.
const (
	RoleOwner   = "owner"
	RoleAdmin   = "admin"
	RoleMember  = "member"
	RoleBilling = "billing"
)

// MembershipRoleValid reports whether role is one of the four roles.
func MembershipRoleValid(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleMember, RoleBilling:
		return true
	}
	return false
}

// InviteRoleValid reports whether role may be granted via an invite
// (owner is never granted through an invite).
func InviteRoleValid(role string) bool {
	switch role {
	case RoleAdmin, RoleMember, RoleBilling:
		return true
	}
	return false
}

// User is a row of users (the global directory; no org scope).
type User struct {
	ID        string    `json:"id"`
	OIDCSub   string    `json:"-"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Org is a row of orgs.
type Org struct {
	ID     string `json:"id"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	PlanID string `json:"plan_id"`
}

// Member is a membership row joined with the user's display fields.
type Member struct {
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	Email  string `json:"email"`
	Name   string `json:"name"`
}

// Project is a row of projects.
type Project struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
}

// Invite is a row of invites (TokenHash never leaves the server; the
// plaintext token is returned once by CreateInvite).
type Invite struct {
	ID       string    `json:"id"`
	OrgID    string    `json:"org_id"`
	Email    string    `json:"email"`
	Role     string    `json:"role"`
	Expires  time.Time `json:"expires_at"`
	Accepted bool      `json:"accepted"`
}

// ErrConflict marks unique-violation style rejections.
var ErrConflict = errors.New("conflict")

// ErrNotFound marks missing rows (callers answer 404 to avoid
// revealing existence, PORTAL_PLAN 5).
var ErrNotFound = errors.New("not found")

// UpsertUser inserts or updates the user identified by the OIDC
// subject and reports whether the row was just created (first login
// triggers personal-org creation, plan 3.1).
func UpsertUser(ctx context.Context, tx pgx.Tx, sub, email, name string) (User, bool, error) {
	u := User{OIDCSub: sub, Email: email, Name: name}
	var created bool
	err := tx.QueryRow(ctx, `
                INSERT INTO users (oidc_sub, email, name)
                VALUES ($1, $2, $3)
                ON CONFLICT (oidc_sub) DO UPDATE
                    SET email = EXCLUDED.email,
                        name  = EXCLUDED.name
                RETURNING id::text, email::text, name, created_at, (xmax = 0)`,
		sub, email, name).Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt, &created)
	if err != nil {
		return User{}, false, fmt.Errorf("upsert user: %w", err)
	}
	return u, created, nil
}

// UserByID loads the user row (users carry no org; no RLS).
func UserByID(ctx context.Context, tx pgx.Tx, userID string) (User, error) {
	u := User{}
	err := tx.QueryRow(ctx, `
                SELECT id::text, oidc_sub, email::text, name, created_at
                FROM users WHERE id = $1`, userID).
		Scan(&u.ID, &u.OIDCSub, &u.Email, &u.Name, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("load user: %w", err)
	}
	return u, nil
}

// CreateOrg inserts the org with a caller-generated id (the RLS
// context is pinned to the same id inside BootstrapTx) and makes
// ownerUserID the owner.
func CreateOrg(ctx context.Context, tx pgx.Tx, orgID, slug, name, ownerUserID string) (Org, error) {
	o := Org{ID: orgID, Slug: slug, Name: name, PlanID: "free"}
	err := tx.QueryRow(ctx, `
                INSERT INTO orgs (id, slug, name) VALUES ($1, $2, $3)
                RETURNING id::text, slug, name, plan_id`,
		orgID, slug, name).Scan(&o.ID, &o.Slug, &o.Name, &o.PlanID)
	if err != nil {
		return Org{}, fmt.Errorf("insert org: %w", err)
	}
	if _, err := tx.Exec(ctx, `
                INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)`,
		orgID, ownerUserID, RoleOwner); err != nil {
		return Org{}, fmt.Errorf("insert owner membership: %w", err)
	}
	return o, nil
}

// ListMyOrgs returns the orgs the user belongs to (RLS does the
// scoping — see orgs_tenant policy).
func ListMyOrgs(ctx context.Context, tx pgx.Tx) ([]Org, error) {
	rows, err := tx.Query(ctx, `
                SELECT o.id::text, o.slug, o.name, o.plan_id
                FROM orgs o ORDER BY o.created_at, o.id`)
	if err != nil {
		return nil, fmt.Errorf("list orgs: %w", err)
	}
	defer rows.Close()
	var out []Org
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.PlanID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MembershipRole returns the caller's role in orgID, ErrNotFound
// when not a member (handlers map that to 404).
func MembershipRole(ctx context.Context, tx pgx.Tx, orgID, userID string) (string, error) {
	var role string
	err := tx.QueryRow(ctx, `
                SELECT role FROM memberships WHERE org_id = $1 AND user_id = $2`,
		orgID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load membership: %w", err)
	}
	return role, nil
}

// ListMembers returns the org's members (app.org_id context).
func ListMembers(ctx context.Context, tx pgx.Tx) ([]Member, error) {
	rows, err := tx.Query(ctx, `
                SELECT m.org_id::text, m.user_id::text, m.role, u.email::text, u.name
                FROM memberships m JOIN users u ON u.id = m.user_id
                ORDER BY m.created_at, m.user_id`)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.OrgID, &m.UserID, &m.Role, &m.Email, &m.Name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMemberRole changes a member's role; only admins/owners may call
// it (checked by the handler), and the owner row is protected here.
func SetMemberRole(ctx context.Context, tx pgx.Tx, orgID, userID, role string) error {
	ct, err := tx.Exec(ctx, `
                UPDATE memberships SET role = $3
                WHERE org_id = $1 AND user_id = $2 AND role <> 'owner'`,
		orgID, userID, role)
	if err != nil {
		return fmt.Errorf("update membership: %w", err)
	}
	if ct.RowsAffected() == 0 {
		// Either the member is gone or the row was the owner's.
		var isOwner bool
		err := tx.QueryRow(ctx, `
                        SELECT EXISTS (SELECT 1 FROM memberships
                                       WHERE org_id = $1 AND user_id = $2 AND role = 'owner')`,
			orgID, userID).Scan(&isOwner)
		if err == nil && isOwner {
			return errors.New("the owner role cannot be changed")
		}
		return ErrNotFound
	}
	return nil
}

// CreateInvite stores an invite and returns the plaintext token —
// shown once on the dashboard, only its sha256 is persisted
// (PORTAL_PLAN 1 #5).
func CreateInvite(ctx context.Context, tx pgx.Tx, orgID, email, role, createdBy string, ttl time.Duration) (Invite, string, error) {
	token := randomToken()
	inv := Invite{OrgID: orgID, Email: email, Role: role, Expires: time.Now().Add(ttl)}
	err := tx.QueryRow(ctx, `
                INSERT INTO invites (org_id, email, role, token_hash, expires_at, created_by)
                VALUES ($1, $2, $3, $4, $5, $6)
                RETURNING id::text, accepted_at IS NOT NULL`,
		orgID, email, role, HashToken(token), inv.Expires, createdBy).
		Scan(&inv.ID, &inv.Accepted)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Invite{}, "", fmt.Errorf("%w: email already invited", ErrConflict)
		}
		return Invite{}, "", fmt.Errorf("insert invite: %w", err)
	}
	return inv, token, nil
}

// AcceptInvite exchanges the plaintext token for a membership in
// orgID (the handler pins app.org_id = orgID first; the invite link
// carries the org id — the token is the secret, plan 3.4).
func AcceptInvite(ctx context.Context, tx pgx.Tx, orgID, token, userID string) (string, error) {
	var role string
	err := tx.QueryRow(ctx, `
                UPDATE invites SET accepted_at = now()
                WHERE org_id = $1 AND token_hash = $2
                  AND accepted_at IS NULL AND expires_at > now()
                RETURNING role`,
		orgID, HashToken(token)).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("accept invite: %w", err)
	}
	if _, err := tx.Exec(ctx, `
                INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)`,
		orgID, userID, role); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return "", fmt.Errorf("%w: already a member", ErrConflict)
		}
		return "", fmt.Errorf("insert membership: %w", err)
	}
	return role, nil
}

// CreateProject inserts a project into the current org context.
func CreateProject(ctx context.Context, tx pgx.Tx, slug, name string) (Project, error) {
	p := Project{Slug: slug, Name: name}
	err := tx.QueryRow(ctx, `
                INSERT INTO projects (org_id, slug, name)
                VALUES (current_setting('app.org_id')::uuid, $1, $2)
                RETURNING id::text, org_id::text`,
		slug, name).Scan(&p.ID, &p.OrgID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Project{}, fmt.Errorf("%w: slug already used", ErrConflict)
		}
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	return p, nil
}

// ListProjects returns the org's projects (app.org_id context).
func ListProjects(ctx context.Context, tx pgx.Tx) ([]Project, error) {
	rows, err := tx.Query(ctx, `
                SELECT id::text, org_id::text, slug, name FROM projects
                ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Slug, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Audit records an audit event into the current org context
// (actorType: "user"|"system"; metadata is marshalled JSON).
func Audit(ctx context.Context, tx pgx.Tx, actorType, actorID, action, target, metadata string) error {
	_, err := tx.Exec(ctx, `
                INSERT INTO audit_events (org_id, actor_type, actor_id, action, target, metadata)
                VALUES (current_setting('app.org_id')::uuid, $1, $2, $3, $4, $5::jsonb)`,
		actorType, actorID, action, target, metadata)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

// AuditEvent is one row of the org's audit trail.
type AuditEvent struct {
	ID        string    `json:"id"`
	ActorType string    `json:"actor_type"`
	ActorID   string    `json:"actor_id"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Metadata  string    `json:"metadata"`
	OccuredAt time.Time `json:"occurred_at"`
}

// ListAudit returns the org's most recent audit events.
func ListAudit(ctx context.Context, tx pgx.Tx, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := tx.Query(ctx, `
                SELECT id::text, actor_type, actor_id, action, target, metadata::text, occurred_at
                FROM audit_events ORDER BY occurred_at DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.ID, &e.ActorType, &e.ActorID, &e.Action, &e.Target, &e.Metadata, &e.OccuredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Slugify normalises a name into a URL-safe slug.
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "org"
	}
	return s
}

// UniqueOrgSlug finds an unused org slug by appending a short random
// suffix on collisions.
func UniqueOrgSlug(ctx context.Context, tx pgx.Tx, base string) (string, error) {
	for i := 0; i < 6; i++ {
		candidate := base
		if i > 0 {
			candidate = base + "-" + randomSuffix()
		}
		var taken bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM orgs WHERE slug = $1)`, candidate).Scan(&taken); err != nil {
			return "", fmt.Errorf("check slug: %w", err)
		}
		if !taken {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: could not derive a free slug", ErrConflict)
}

// HashToken is the one-way form stored for invite tokens.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("tenants: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func randomSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("tenants: crypto/rand failed: " + err.Error())
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
