// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"context"
	"sync"
)

type LocktabEntry[K comparable] struct {
	// held is the entry's lock: a token in the buffer means it is held. A
	// channel rather than a sync.Mutex so AcquireCtx can give up waiting.
	held chan struct{}
	p    *Locktab[K]
	name K

	// protected with p.Mutex !!
	c int
}

func (e *LocktabEntry[K]) inc() {
	e.c++
}

func (e *LocktabEntry[K]) dec() {
	e.c--
}

type Locktab[K comparable] struct {
	sync.Mutex
	m map[K]*LocktabEntry[K]
}

// ref finds or makes k's entry and counts the caller as one of its users,
// so the entry outlives this caller's wait for it.
func (l *Locktab[K]) ref(k K) *LocktabEntry[K] {
	l.Lock()
	defer l.Unlock()
	if l.m == nil {
		l.m = make(map[K]*LocktabEntry[K])
	}
	var ret *LocktabEntry[K]
	if ret = l.m[k]; ret == nil {
		ret = &LocktabEntry[K]{
			held: make(chan struct{}, 1),
			p:    l,
			name: k,
		}
		l.m[k] = ret
	}
	ret.inc()
	return ret
}

// unref drops one user of e, deleting it from the table with the last.
func (e *LocktabEntry[K]) unref() {
	parent := e.p
	parent.Lock()
	defer parent.Unlock()
	e.dec()
	if e.c == 0 {
		delete(parent.m, e.name)
	}
}

func (l *Locktab[K]) Acquire(k K) *LocktabEntry[K] {
	ret := l.ref(k)
	ret.held <- struct{}{}
	return ret
}

// AcquireCtx is Acquire that gives up when ctx is done, returning ctx's
// error and holding nothing.
func (l *Locktab[K]) AcquireCtx(ctx context.Context, k K) (*LocktabEntry[K], error) {
	ret := l.ref(k)
	select {
	case ret.held <- struct{}{}:
		return ret, nil
	case <-ctx.Done():
		ret.unref()
		return nil, ctx.Err()
	}
}

func (e *LocktabEntry[K]) Release() {
	<-e.held
	e.unref()
}
