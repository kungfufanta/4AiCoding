// Package passwordauth implements the local username/password login:
// the bootstrap admin account lives in password_logins, hashed with
// bcrypt. It exists so the dashboard is reachable before an OIDC
// provider is configured (owner decision 2026-11-11, docs/PORTAL_PLAN.md
// §2); SSO stays the primary path and there is no self-service signup.
package passwordauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// ErrNotFound marks a username with no password login (callers answer
// 401 without revealing whether the account exists).
var ErrNotFound = errors.New("password login not found")

// dummyHash equalizes login timing when the username is unknown: the
// handler still runs one bcrypt comparison against it, so a missing
// account and a wrong password cost about the same. A valid bcrypt
// digest of "dummy-password-for-timing" (cost 10).
const dummyHash = "$2a$10$Vz1BgkbQvPQRWoS3gyXuoeZ1pxS1wpnVaS7.uD3CTHpkSQ1RsxoNW"

// SubjectPrefix tags the users.oidc_sub of password identities so a
// future SSO account can never collide with the local namespace.
const SubjectPrefix = "password:"

// Lookup returns the user id and bcrypt hash stored for username
// (CITEXT: the comparison is case-insensitive).
func Lookup(ctx context.Context, pool *pgxpool.Pool, username string) (userID, hash string, err error) {
	err = pool.QueryRow(ctx,
		`SELECT user_id::text, password_hash FROM password_logins WHERE username = $1`,
		strings.TrimSpace(username)).Scan(&userID, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("lookup password login: %w", err)
	}
	return userID, hash, nil
}

// Verify reports whether password matches a bcrypt hash. Cost is
// stored in the hash itself.
func Verify(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// VerifyDummy runs one bcrypt comparison against a fixed hash; use it
// when the username is unknown to keep the response time similar to
// the wrong-password path.
func VerifyDummy(password string) bool {
	return Verify(dummyHash, password)
}

// Hash hashes password with bcrypt (DefaultCost).
func Hash(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// EnsureBootstrap makes sure the configured admin username exists as a
// password login carrying the configured password, creating the user
// row and personal org on first boot. It runs in one BootstrapTx with
// the same shape as the SSO first-login bootstrap (httpapi
// bootstrapLogin), so admin starts with a personal org like any other
// user.
//
// Idempotent and converging: safe on every boot, and the stored hash
// always ends up matching the configured password — that is how
// PORTAL_ADMIN_PASSWORD rotation is applied (set a new value,
// redeploy; no SQL by hand).
//
// Returns the admin user id (A2: needed by httpapi.Server to gate the
// /api/admin/llm/* routes against the session user).
func EnsureBootstrap(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, username, password, email string) (string, error) {
	hash, err := Hash(password)
	if err != nil {
		return "", err
	}
	newOrgID, err := db.NewOrgID()
	if err != nil {
		return "", err
	}
	sub := SubjectPrefix + strings.ToLower(strings.TrimSpace(username))
	var adminID string
	err = db.BootstrapTx(ctx, pool, newOrgID, func(tx db.Tx) error {
		u, _, err := tenants.UpsertUser(ctx, tx, sub, email, "Admin")
		if err != nil {
			if errors.Is(err, tenants.ErrEmailInUse) {
				return fmt.Errorf("admin email %q is taken by another account; set PORTAL_ADMIN_EMAIL", email)
			}
			return err
		}
		adminID = u.ID
		if err := db.PinUser(ctx, tx, u.ID); err != nil {
			return err
		}
		n, err := tenants.CountMyMemberships(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			org, err := tenants.CreateOrg(ctx, tx, newOrgID, tenants.Slugify(username), "Admin's org", u.ID)
			if err != nil {
				return err
			}
			meta := fmt.Sprintf(`{"org_id":%q,"slug":%q}`, newOrgID, org.Slug)
			if err := tenants.Audit(ctx, tx, "user", u.ID, "org.create", newOrgID, meta); err != nil {
				return err
			}
		}
		// Rotation: when the configured password no longer matches the
		// stored hash, sign out every session of this account, so a
		// session opened with the old password does not outlive it.
		var oldHash string
		err = tx.QueryRow(ctx, `SELECT password_hash FROM password_logins WHERE username = $1`,
			strings.TrimSpace(username)).Scan(&oldHash)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read password login: %w", err)
		case !Verify(oldHash, password):
			if _, err := tx.Exec(ctx, `DELETE FROM web_sessions WHERE user_id = $1::uuid`, u.ID); err != nil {
				return fmt.Errorf("revoke sessions after rotation: %w", err)
			}
			log.Info("bootstrap admin password rotated; existing sessions revoked")
		}
		_, err = tx.Exec(ctx, `
                        INSERT INTO password_logins (user_id, username, password_hash)
                        VALUES ($1::uuid, $2, $3)
                        ON CONFLICT (username) DO UPDATE
                            SET user_id       = EXCLUDED.user_id,
                                password_hash = EXCLUDED.password_hash`,
			u.ID, strings.TrimSpace(username), hash)
		if err != nil {
			return fmt.Errorf("upsert password login: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	log.Info("bootstrap password login ensured", "username", strings.TrimSpace(username))
	return adminID, nil
}

// DisableAll removes every password login and signs out the sessions
// of those accounts. Run at boot when PORTAL_ADMIN_PASSWORD is unset,
// so a hash stored by an earlier boot (e.g. the old admin/admin123
// default) cannot be used again if the endpoint is re-enabled, and so
// sessions opened with it end now instead of after their TTL.
func DisableAll(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sessions, err := tx.Exec(ctx, `
                DELETE FROM web_sessions
                WHERE user_id IN (SELECT user_id FROM password_logins)`)
	if err != nil {
		return fmt.Errorf("revoke password-login sessions: %w", err)
	}
	logins, err := tx.Exec(ctx, `DELETE FROM password_logins`)
	if err != nil {
		return fmt.Errorf("delete password logins: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if n := logins.RowsAffected(); n > 0 {
		log.Info("password login disabled; removed stored logins",
			"logins", n, "sessions_revoked", sessions.RowsAffected())
	}
	return nil
}
