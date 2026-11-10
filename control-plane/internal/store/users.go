package store

import (
	"context"
	"database/sql"
	"time"
)

// User is a console / API user (migrations/0029).
type User struct {
	ID           string
	Username     string
	PasswordHash string
	Role         string // admin | operator | viewer
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const userCols = `id, username, password_hash, role, disabled, created_at, updated_at`

func scanUser(sc scanner) (*User, error) {
	u := &User{}
	var created, updated int64
	var disabled int
	err := sc.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &disabled, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	u.CreatedAt = time.Unix(created, 0).UTC()
	u.UpdatedAt = time.Unix(updated, 0).UTC()
	return u, nil
}

// GetUser returns a user by id.
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM app_user WHERE id = ?`, id))
}

// GetUserByName returns a user by username (case-insensitive).
func (s *Store) GetUserByName(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM app_user WHERE username = ?`, username))
}

// ListUsers returns every user, oldest first.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM app_user ORDER BY created_at, username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of users (0 = first run).
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM app_user`).Scan(&n)
	return n, err
}

// CountActiveAdmins returns the number of enabled admins.
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM app_user WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// CreateUser inserts a user. ErrConflict when the username is taken.
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	now := time.Now().UTC()
	u.CreatedAt, u.UpdatedAt = now, now
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO app_user (id, username, password_hash, role, disabled, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?)`, u.ID, u.Username, u.PasswordHash, u.Role, boolInt(u.Disabled), now.Unix(), now.Unix())
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

// UpdateUser writes role, disabled and password_hash back.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	u.UpdatedAt = time.Now().UTC()
	return s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx, `
			UPDATE app_user SET role = ?, disabled = ?, password_hash = ?, updated_at = ? WHERE id = ?`,
			u.Role, boolInt(u.Disabled), u.PasswordHash, u.UpdatedAt.Unix(), u.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteUser removes a user (their sessions cascade).
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		if _, err := db.ExecContext(ctx, `DELETE FROM console_session WHERE user_id = ?`, id); err != nil {
			return err
		}
		res, err := db.ExecContext(ctx, `DELETE FROM app_user WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteAllUsers removes every user and session: the console returns to the
// first-run "create the admin" screen (operator recovery with an API key).
func (s *Store) DeleteAllUsers(ctx context.Context) error {
	return s.submit(ctx, func(db *sql.DB) error {
		if _, err := db.ExecContext(ctx, `DELETE FROM console_session`); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, `DELETE FROM app_user`)
		return err
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
