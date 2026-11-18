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

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is a per-user token bucket, in memory. Per process is enough
// while one web server serves a host; behind several, each enforces its own
// share, which is still a bound.
type rateLimiter struct {
	sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[rateKey]*bucket
	sweep   time.Time
}

func newRateLimiter(rate, burst float64, now func() time.Time) *rateLimiter {
	return &rateLimiter{
		rate:    rate,
		burst:   burst,
		now:     now,
		buckets: make(map[rateKey]*bucket),
		sweep:   now(),
	}
}

// allow takes one token for k. When none is left it reports how long until
// one will be.
func (l *rateLimiter) allow(k rateKey) (time.Duration, bool) {
	l.Lock()
	defer l.Unlock()
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
	if b.tokens < 1 {
		need := (1 - b.tokens) / l.rate
		return time.Duration(need * float64(time.Second)), false
	}
	b.tokens--
	return 0, true
}
