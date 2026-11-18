// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package api

import (
	"sync"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
)

const (
	// A client flushes analytics every minute or so; a few requests a second
	// with a burst of 30 is far above honest use and caps a runaway loop.
	defaultRatePerSec = 2.0
	defaultBurst      = 30.0

	// limiterIdle is how long an unused bucket is kept. A full bucket after
	// this long is indistinguishable from a fresh one, so dropping it is free.
	limiterIdle = 10 * time.Minute
)

type rateKey struct {
	shid core.ShortHostID
	uid  proto.UID
}

const (
	// Failed bearer checks per client IP. Charged only when a token is
	// refused, and checked BEFORE the database lookup, so a flood of made-up
	// tokens is turned away without touching the pool. Honest clients almost
	// never fail, so the budget can be small.
	failRatePerSec = 1.0
	failBurst      = 20.0
)

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is a per-user token bucket, in memory. Per process is enough
// while one web server serves a host; behind several, each enforces its own
// share, which is still a bound.
type rateLimiter[K comparable] struct {
	sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[K]*bucket
	sweep   time.Time
}

func newRateLimiter[K comparable](rate, burst float64, now func() time.Time) *rateLimiter[K] {
	return &rateLimiter[K]{
		rate:    rate,
		burst:   burst,
		now:     now,
		buckets: make(map[K]*bucket),
		sweep:   now(),
	}
}

// refill brings k's bucket up to date and returns it, sweeping idle buckets
// now and then. Callers hold the lock.
func (l *rateLimiter[K]) refill(k K) *bucket {
	now := l.now()
	if now.Sub(l.sweep) > limiterIdle {
		for key, b := range l.buckets {
			if now.Sub(b.last) > limiterIdle {
				delete(l.buckets, key)
			}
		}
		l.sweep = now
	}
	b, ok := l.buckets[k]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[k] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	return b
}

func (l *rateLimiter[K]) wait(b *bucket) time.Duration {
	return time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// allow takes one token for k. When none is left it reports how long until
// one will be.
func (l *rateLimiter[K]) allow(k K) (time.Duration, bool) {
	l.Lock()
	defer l.Unlock()
	b := l.refill(k)
	if b.tokens < 1 {
		return l.wait(b), false
	}
	b.tokens--
	return 0, true
}

// check reports whether k has a token left, without taking it.
func (l *rateLimiter[K]) check(k K) (time.Duration, bool) {
	l.Lock()
	defer l.Unlock()
	b := l.refill(k)
	if b.tokens < 1 {
		return l.wait(b), false
	}
	return 0, true
}

// charge takes a token from k, going below zero if need be, so a client that
// keeps failing while blocked stays blocked.
func (l *rateLimiter[K]) charge(k K) {
	l.Lock()
	defer l.Unlock()
	b := l.refill(k)
	b.tokens = max(b.tokens-1, -l.burst)
}
