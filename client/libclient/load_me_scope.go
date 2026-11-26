// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"context"
	"errors"
	"sync"

	proto "github.com/foks-proj/go-foks/proto/lib"
)

// A load-me scope lets the steps of one operation share a single load of the
// user's own chain. A team listing reaches it twice -- the membership loader
// and the PUK minder -- and each load is a server round trip returning the
// same chain. The scope lives in the operation's context, so it ends with the
// operation: nothing loaded in one is served to another, however close
// together.
//
// A scope is for READ-ONLY operations on the user's sigchain: a chain loaded
// at the start is served to every later step, so a step that posted to the
// sigchain would be handed the chain from before its own write. That is why
// the constructor is unexported and there is no way to "refresh" a scope: the
// only operations that open one are listed here, and none of them writes the
// user's sigchain.
//
//   - TeamMinder.ListMemberships (an exploration: chain loads, generic-chain
//     reads and team-membership-chain posts, never a user sigchain link).
//   - UserContext.UnlockKeys (loads my chain while the SSO ping is out, so
//     PopulateWithDevkey joins that load; unlocking reads keys, it posts no
//     sigchain link).

type loadMeScopeKey struct{}

type loadMeScope struct {
	mu      sync.Mutex
	loads   map[proto.UID]*UserWrapper
	flights map[proto.UID]*loadMeFlight

	// onWait, if set (tests only), runs when a caller starts waiting on a
	// load in flight.
	onWait func()
}

type loadMeFlight struct {
	done chan struct{}
	res  *UserWrapper
	err  error
}

// errLoadMeAbandoned is a flight's outcome until its load returns. A load that
// never returns normally (it panicked) leaves it in place, so the flight
// counts as failed: nothing is shared, and its waiters load for themselves.
var errLoadMeAbandoned = errors.New("load-me flight abandoned")

func newLoadMeScope() *loadMeScope {
	return &loadMeScope{
		loads:   make(map[proto.UID]*UserWrapper),
		flights: make(map[proto.UID]*loadMeFlight),
	}
}

// withLoadMeScope starts a load-me scope for one read-only operation; see the
// list above before adding a caller.
func withLoadMeScope(m MetaContext) MetaContext {
	ret := m
	ret.MetaContext = m.MetaContext.WithContext(
		context.WithValue(m.Ctx(), loadMeScopeKey{}, newLoadMeScope()))
	return ret
}

func loadMeScopeOf(m MetaContext) *loadMeScope {
	s, _ := m.Ctx().Value(loadMeScopeKey{}).(*loadMeScope)
	return s
}

// loadMeScoped is LoadMe that shares one load per user within the
// operation's load-me scope, if it has one; outside a scope it is LoadMe.
// Concurrent callers wait for the load in flight and share its outcome. A
// failure is shared too -- the waiters belong to the same operation, and a
// failure retried at once (offline, say) would only cost another dial --
// unless it is the loader's own cancellation or the load panicked: then the
// waiter loads for itself. Nothing failed is kept for later callers.
func loadMeScoped(m MetaContext, au *UserContext) (*UserWrapper, error) {
	scope := loadMeScopeOf(m)
	if au == nil {
		au = m.G().ActiveUser()
	}
	if scope == nil || au == nil {
		return LoadMe(m, au)
	}
	return scope.load(m.Ctx(), au.Info.Fqu.Uid, func() (*UserWrapper, error) {
		return LoadMe(m, au)
	})
}

// load returns the scope's load of uid, running fn for it if there is none.
// Concurrent callers wait for the load in flight and share its outcome; see
// loadMeScoped for when a waiter loads for itself instead.
func (s *loadMeScope) load(
	ctx context.Context,
	uid proto.UID,
	fn func() (*UserWrapper, error),
) (*UserWrapper, error) {
	s.mu.Lock()
	if res, ok := s.loads[uid]; ok {
		s.mu.Unlock()
		return res, nil
	}
	if f, ok := s.flights[uid]; ok {
		s.mu.Unlock()
		if s.onWait != nil {
			s.onWait()
		}
		select {
		case <-f.done:
			if f.err == nil {
				return f.res, nil
			}
			if errors.Is(f.err, errLoadMeAbandoned) ||
				errors.Is(f.err, context.Canceled) ||
				errors.Is(f.err, context.DeadlineExceeded) {
				return fn()
			}
			return nil, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &loadMeFlight{done: make(chan struct{}), err: errLoadMeAbandoned}
	s.flights[uid] = f
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.flights, uid)
		if f.err == nil {
			s.loads[uid] = f.res
		}
		s.mu.Unlock()
		close(f.done)
	}()
	f.res, f.err = fn()
	return f.res, f.err
}
