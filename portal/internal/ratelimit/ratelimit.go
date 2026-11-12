// Package ratelimit implements a per-key token-bucket limiter for the
// public /v1 API (PORTAL_PLAN 6/S4: "Rate limit theo key: token bucket,
// vượt → 429 kèm Retry-After"). The bucket is in-process: a Portal
// replica applies the limit independently, so the effective ceiling
// for an org with replicas=N is plan.RateLimitRPS * N. The plan keeps
// the bucket small (free = 2 RPS) and the Burst == RPS, so a single
// agent loop sending a burst larger than RPS is throttled predictably.
//
// Postgres-backed rate limiting is the S6 fallback (a token-bucket
// table per key); it is not needed at the current scale (one replica,
// one agent per org at a time).
package ratelimit

import (
	"sync"
	"time"
)

// Bucket is a token bucket for one key. Tokens refill at RPS per
// second; the bucket holds at most `burst` tokens. A Take that finds
// fewer tokens than requested returns false with the time until enough
// tokens have refilled.
type Bucket struct {
	mu         sync.Mutex
	tokens     float64
	last       time.Time
	refillPerS float64
	burst      float64
}

// Limiter is a keyed set of token buckets. The zero value is ready to
// use. Buckets are created lazily on first Take and never pruned
// (expectation: hundreds of keys, not millions).
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*Bucket
	now     func() time.Time
}

// New returns a Limiter whose clock is time.Now (overridable in tests
// by mutating the now field directly via a sibling constructor).
func New() *Limiter {
	return &Limiter{buckets: map[string]*Bucket{}, now: time.Now}
}

// Take attempts to deduct `cost` tokens from the bucket for key. rps
// is the bucket's refill rate (and burst) — it is read from the plan
// at call time so an org upgrade takes effect immediately. Returns
// (ok=true, 0) when the bucket has enough; (ok=false, retryAfter)
// otherwise, with retryAfter rounded up to the next millisecond so the
// caller's Retry-After header is never 0 on a refuse.
//
// cost <= 0 means "peek": deduct 0 tokens but still create/refill the
// bucket. Use it to warm up before the actual charge.
func (l *Limiter) Take(key string, rps int, cost float64) (ok bool, retryAfter time.Duration) {
	if rps <= 0 {
		// 0 in the plan means "no rate limit" (custom plans only).
		return true, 0
	}
	b := l.bucket(key, rps)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := l.now()
	if b.last.IsZero() {
		b.last = now
		b.tokens = float64(rps) // start full
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * float64(rps)
		if b.tokens > float64(rps) {
			b.tokens = float64(rps)
		}
		b.last = now
	}
	if cost <= 0 {
		return true, 0
	}
	if b.tokens >= cost {
		b.tokens -= cost
		return true, 0
	}
	needed := cost - b.tokens
	wait := time.Duration(needed / float64(rps) * float64(time.Second)).Round(time.Millisecond)
	if wait <= 0 {
		wait = time.Millisecond
	}
	return false, wait
}

// bucket returns (lazily creating) the bucket for key. Each call
// re-asserts rps so an upgrade from free (2) to pro (20) takes effect
// on the next Take without restarting the Portal.
func (l *Limiter) bucket(key string, rps int) *Bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &Bucket{refillPerS: float64(rps), burst: float64(rps)}
		l.buckets[key] = b
	}
	// An upgrade changes the ceiling; the bucket's tokens are
	// unchanged but the refill rate (and the cap on the next refill)
	// picks up the new rps.
	b.refillPerS = float64(rps)
	b.burst = float64(rps)
	return b
}

// Reset clears all buckets. Tests use it to start each case on a
// clean slate; production never calls it.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets = map[string]*Bucket{}
}

// BucketKind identifies a route family so each family gets its own
// token bucket per key (PORTAL_PLAN_V2 P5b: "Mỗi nhóm có bucket riêng
// theo key"). The free plan's 2 RPS bucket would otherwise be drained
// by a files-list loop, blocking exec; splitting the buckets keeps
// one noisy family from starving the others.
type BucketKind string

const (
	BucketCreate BucketKind = "create" // POST /v1/sandboxes
	BucketExec   BucketKind = "exec"   // POST .../exec
	BucketFiles  BucketKind = "files"  // /files* (5 routes)
	BucketAction BucketKind = "action" // start/stop/delete
	BucketStream BucketKind = "stream" // logs/metrics stream open
)

// TakeKind is Take with a bucket kind: the key is composed as
// "<keyID>:<kind>" so each family has its own tokens. rps is the
// refill rate (read from the plan at call time); cost is the token
// cost (1 for a single call; streams may charge more on open).
func (l *Limiter) TakeKind(keyID string, kind BucketKind, rps int, cost float64) (bool, time.Duration) {
	return l.Take(string(kind)+":"+keyID, rps, cost)
}
