package ratelimit

import (
	"testing"
	"time"
)

// fakeClock is a controllable time source for the bucket's last/now.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

// newLimiter returns a Limiter whose clock is the fakeClock; mutating
// the clock advances every subsequent Take.
func newLimiter(t *testing.T) (*Limiter, *fakeClock) {
	t.Helper()
	fc := &fakeClock{t: time.UnixMilli(1_700_000_000_000)}
	l := New()
	l.now = fc.now
	return l, fc
}

func TestTakeAllowsUpToBurst(t *testing.T) {
	l, _ := newLimiter(t)
	// free plan = 2 RPS, burst == 2 — first two Takes at t0 succeed.
	if ok, _ := l.Take("k1", 2, 1); !ok {
		t.Fatalf("first take should succeed")
	}
	if ok, _ := l.Take("k1", 2, 1); !ok {
		t.Fatalf("second take should succeed")
	}
	if ok, _ := l.Take("k1", 2, 1); ok {
		t.Fatalf("third take at the same instant should be refused (burst exhausted)")
	}
}

func TestTakeRefillsOverTime(t *testing.T) {
	l, fc := newLimiter(t)
	l.Take("k1", 2, 1)
	l.Take("k1", 2, 1)
	// Move the clock forward by 500ms — at 2 RPS that is one new token.
	fc.t = fc.t.Add(500 * time.Millisecond)
	if ok, retry := l.Take("k1", 2, 1); !ok {
		t.Fatalf("take after 500ms (1 token) should succeed, got retry=%v", retry)
	}
	// And then we are empty again at this instant.
	if ok, _ := l.Take("k1", 2, 1); ok {
		t.Fatalf("take right after should be refused (no token yet)")
	}
}

func TestRetryAfterIsPositive(t *testing.T) {
	l, _ := newLimiter(t)
	l.Take("k1", 2, 1)
	l.Take("k1", 2, 1)
	ok, retry := l.Take("k1", 2, 1)
	if ok {
		t.Fatalf("third take should be refused")
	}
	if retry <= 0 {
		t.Fatalf("retryAfter must be > 0 on a refuse, got %v", retry)
	}
}

func TestZeroRPSMeansUnlimited(t *testing.T) {
	l, _ := newLimiter(t)
	for i := 0; i < 100; i++ {
		if ok, _ := l.Take("k1", 0, 1); !ok {
			t.Fatalf("take %d with rps=0 should always succeed", i)
		}
	}
}

func TestKeysAreIsolated(t *testing.T) {
	l, _ := newLimiter(t)
	// Burn through k1's bucket.
	l.Take("k1", 2, 1)
	l.Take("k1", 2, 1)
	// k2's bucket is independent and starts full.
	if ok, _ := l.Take("k2", 2, 1); !ok {
		t.Fatalf("take on a fresh key should succeed")
	}
}

func TestCostZeroPeeks(t *testing.T) {
	l, _ := newLimiter(t)
	// Peek should not deduct a token.
	if ok, _ := l.Take("k1", 2, 0); !ok {
		t.Fatalf("peek should always succeed")
	}
	if ok, _ := l.Take("k1", 2, 1); !ok {
		t.Fatalf("take after a peek should still have a full bucket")
	}
}

func TestUpgradeRaisesCeiling(t *testing.T) {
	l, _ := newLimiter(t)
	// Burn free plan (2 RPS): two takes succeed, third refused.
	l.Take("k1", 2, 1)
	l.Take("k1", 2, 1)
	if ok, _ := l.Take("k1", 2, 1); ok {
		t.Fatalf("third take at free rps should be refused")
	}
	// Upgrade the org's plan to pro (20 RPS) without restarting: the
	// next Take sees the new rps. The bucket's tokens are unchanged
	// (still empty), so the first Take at the new rps is still
	// refused, but a Take after a 100ms wait (2 tokens refilled at
	// 20 RPS) succeeds.
	l.now = func() time.Time { return time.UnixMilli(1_700_000_000_100) }
	// Re-arm the test clock so the next refill computation works.
	fc := &fakeClock{t: time.UnixMilli(1_700_000_000_000)}
	l.now = fc.now
	// At the original instant, bucket is empty.
	if ok, _ := l.Take("k1", 20, 1); ok {
		t.Fatalf("take immediately after upgrade with empty bucket should be refused")
	}
	// Advance 100ms — at 20 RPS that's 2 tokens refilled.
	fc.t = fc.t.Add(100 * time.Millisecond)
	if ok, _ := l.Take("k1", 20, 1); !ok {
		t.Fatalf("take after 100ms at 20 RPS should succeed")
	}
}
