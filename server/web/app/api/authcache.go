// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package api

import (
	"sync"
	"time"

	"github.com/foks-proj/go-foks/server/shared"
)

const (
	// authCacheTTL is how long a verified token is trusted without asking
	// the database again. It bounds how long a token keeps working after it
	// is deleted (cap rotation, GC) or expires.
	authCacheTTL = 30 * time.Second

	// authCacheMax bounds memory. Past it the cache is emptied wholesale --
	// crude, but only a host with that many active API users ever gets there,
	// and the cost is one extra lookup per user.
	authCacheMax = 10000
)

type authCacheKey [32]byte

type authCacheEntry struct {
	auth  shared.ApiSessionAuth
	until time.Time
}

// authCache remembers recently verified tokens, keyed by token hash, so a
// client's steady stream of requests -- including ones the per-user limiter
// is about to refuse -- costs no database round trip. Only successes are
// cached; a refusal is never remembered, so a token is never wrongly refused.
type authCache struct {
	sync.Mutex
	now     func() time.Time
	entries map[authCacheKey]authCacheEntry
}

func newAuthCache(now func() time.Time) *authCache {
	return &authCache{now: now, entries: make(map[authCacheKey]authCacheEntry)}
}

func (c *authCache) get(k authCacheKey) (*shared.ApiSessionAuth, bool) {
	c.Lock()
	defer c.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.until) {
		delete(c.entries, k)
		return nil, false
	}
	return &e.auth, true
}

// put caches auth until authCacheTTL from now or the token's expiry,
// whichever is first.
func (c *authCache) put(k authCacheKey, auth shared.ApiSessionAuth, expires time.Time) {
	c.Lock()
	defer c.Unlock()
	if len(c.entries) >= authCacheMax {
		clear(c.entries)
	}
	until := c.now().Add(authCacheTTL)
	if expires.Before(until) {
		until = expires
	}
	c.entries[k] = authCacheEntry{auth: auth, until: until}
}
