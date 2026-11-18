// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package api

import (
	"sync"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"golang.org/x/time/rate"
)

const (
	// A client flushes analytics every minute or so; a few requests a second
	// with a burst of 30 is far above honest use and caps a runaway loop.
	defaultRatePerSec = 2.0
	defaultBurst      = 30

	// Failed bearer checks per client IP. Charged only when a token is
	// refused, and checked BEFORE the database lookup, so a flood of made-up
	// tokens is turned away without touching the pool. Honest clients almost
	// never fail, so the budget can be small.
	failRatePerSec = 1.0
	failBurst      = 20

	// limiterIdle is how long an unused limiter is kept. A full bucket after
	// this long is indistinguishable from a fresh one, so dropping it is free.
	limiterIdle = 10 * time.Minute
)

type rateKey struct {
	shid core.ShortHostID
	uid  proto.UID
}

// clientAddr is a client's IP as the TCP peer reports it; a distinct type so
// a uid or token string can't be used as the per-IP key by mistake.
type clientAddr string

type keyedEntry struct {
	lim  *rate.Limiter
	last time.Time
}

// rateLimiter is one golang.org/x/time/rate token bucket per key, in memory,
// with idle keys swept. Per process is enough while one web server serves a
// host; behind several, each enforces its own share, which is still a bound.
// Every method takes the time from `now`, so tests can drive the clock.
type rateLimiter[K comparable] struct {
	sync.Mutex
	rate    rate.Limit
	burst   int
	now     func() time.Time
	entries map[K]*keyedEntry
	sweep   time.Time
}

func newRateLimiter[K comparable](perSec float64, burst int, now func() time.Time) *rateLimiter[K] {
	return &rateLimiter[K]{
		rate:    rate.Limit(perSec),
		burst:   burst,
		now:     now,
		entries: make(map[K]*keyedEntry),
		sweep:   now(),
	}
}

// get returns k's limiter, creating it full, and sweeps idle keys now and
// then. Callers hold the lock.
func (l *rateLimiter[K]) get(k K, now time.Time) *rate.Limiter {
	if now.Sub(l.sweep) > limiterIdle {
		for key, e := range l.entries {
			if now.Sub(e.last) > limiterIdle {
				delete(l.entries, key)
			}
		}
		l.sweep = now
	}
	e, ok := l.entries[k]
	if !ok {
		e = &keyedEntry{lim: rate.NewLimiter(l.rate, l.burst)}
		l.entries[k] = e
	}
	e.last = now
	return e.lim
}

// untilToken is how long until lim has a whole token again.
func (l *rateLimiter[K]) untilToken(lim *rate.Limiter, now time.Time) time.Duration {
	return time.Duration((1 - lim.TokensAt(now)) / float64(l.rate) * float64(time.Second))
}

// allow takes one token for k. When none is left it reports how long until
// one will be.
func (l *rateLimiter[K]) allow(k K) (time.Duration, bool) {
	l.Lock()
	defer l.Unlock()
	now := l.now()
	lim := l.get(k, now)
	if lim.AllowN(now, 1) {
		return 0, true
	}
	return l.untilToken(lim, now), false
}

// check reports whether k has a token left, without taking it.
func (l *rateLimiter[K]) check(k K) (time.Duration, bool) {
	l.Lock()
	defer l.Unlock()
	now := l.now()
	lim := l.get(k, now)
	if lim.TokensAt(now) >= 1 {
		return 0, true
	}
	return l.untilToken(lim, now), false
}

// charge takes a token from k even when none is left, so a client that keeps
// failing while blocked stays blocked. The debt stops at -burst, so a
// blocked client is never shut out longer than two bursts' worth of refill.
func (l *rateLimiter[K]) charge(k K) {
	l.Lock()
	defer l.Unlock()
	now := l.now()
	lim := l.get(k, now)
	if lim.TokensAt(now) > -float64(l.burst) {
		lim.ReserveN(now, 1)
	}
}
