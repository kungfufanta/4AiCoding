package store

import "context"

// CountApps returns the number of apps on this instance (telemetry buckets it).
func (s *Store) CountApps(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM app`).Scan(&n)
	return n, err
}

// CountTasksSince returns how many coding tasks were created at or after the
// given unix time (telemetry buckets it).
func (s *Store) CountTasksSince(ctx context.Context, unix int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task WHERE created_at >= ?`, unix).Scan(&n)
	return n, err
}
