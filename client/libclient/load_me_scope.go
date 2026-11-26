// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"context"
	"sync"

	proto "github.com/foks-proj/go-foks/proto/lib"
)

// A load-me scope lets the steps of one operation share a single load of the
// user's own chain. A team listing reaches it twice -- the membership loader
// and the PUK minder -- and each load is a server round trip returning the
// same chain. The scope lives in the operation's context, so it ends with the
// operation: nothing loaded in one is served to another, however close
// together. A step that changes the user's sigchain inside the scope calls
// ForgetLoadMeScope, so later steps load afresh.

type loadMeScopeKey struct{}

type loadMeScope struct {
	mu      sync.Mutex
	loads   map[proto.UID]*UserWrapper
	flights map[proto.UID]*loadMeFlight
}

type loadMeFlight struct {
	done chan struct{}
	res  *UserWrapper
	err  error
}

// WithLoadMeScope starts a load-me scope for one operation.
func WithLoadMeScope(m MetaContext) MetaContext {
	scope := &loadMeScope{
		loads:   make(map[proto.UID]*UserWrapper),
		flights: make(map[proto.UID]*loadMeFlight),
	}
	ret := m
	ret.MetaContext = m.MetaContext.WithContext(context.WithValue(m.Ctx(), loadMeScopeKey{}, scope))
	return ret
}

func loadMeScopeOf(m MetaContext) *loadMeScope {
	s, _ := m.Ctx().Value(loadMeScopeKey{}).(*loadMeScope)
	return s
}

// LoadMeScoped is LoadMe that shares one load per user within the
// operation's load-me scope, if it has one; outside a scope it is LoadMe.
// Concurrent callers wait for the load in flight; only successful loads are
// shared, since a failure may be the loader's own context's.
func LoadMeScoped(m MetaContext, au *UserContext) (*UserWrapper, error) {
	scope := loadMeScopeOf(m)
	if au == nil {
		au = m.G().ActiveUser()
	}
	if scope == nil || au == nil {
		return LoadMe(m, au)
	}
	uid := au.Info.Fqu.Uid

	scope.mu.Lock()
	if res, ok := scope.loads[uid]; ok {
		scope.mu.Unlock()
		return res, nil
	}
	if f, ok := scope.flights[uid]; ok {
		scope.mu.Unlock()
		select {
		case <-f.done:
			if f.err == nil {
				return f.res, nil
			}
			return LoadMe(m, au)
		case <-m.Ctx().Done():
			return nil, m.Ctx().Err()
		}
	}
	f := &loadMeFlight{done: make(chan struct{})}
	scope.flights[uid] = f
	scope.mu.Unlock()

	defer func() {
		scope.mu.Lock()
		if scope.flights[uid] == f {
			delete(scope.flights, uid)
			if f.err == nil {
				scope.loads[uid] = f.res
			}
		}
		scope.mu.Unlock()
		close(f.done)
	}()
	f.res, f.err = LoadMe(m, au)
	return f.res, f.err
}

// ForgetLoadMeScope drops what the operation's scope holds, and any load in
// flight, so steps after a change to the user's sigchain see the change.
func ForgetLoadMeScope(m MetaContext) {
	scope := loadMeScopeOf(m)
	if scope == nil {
		return
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	clear(scope.loads)
	clear(scope.flights)
}
