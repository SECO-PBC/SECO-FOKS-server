// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/foks-proj/go-ctxlog"
	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func TestLoadMeScopeSharesSuccess(t *testing.T) {
	s := newLoadMeScope()
	var uid proto.UID
	var calls atomic.Int32
	want := &UserWrapper{}
	release := make(chan struct{})
	fn := func() (*UserWrapper, error) {
		calls.Add(1)
		<-release
		return want, nil
	}

	var wg sync.WaitGroup
	got := make([]*UserWrapper, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.load(context.Background(), uid, fn)
			require.NoError(t, err)
			got[i] = res
		}()
	}
	close(release)
	wg.Wait()
	// A caller arriving after the flight is served from the scope.
	res, err := s.load(context.Background(), uid, fn)
	require.NoError(t, err)
	got = append(got, res)

	require.Equal(t, int32(1), calls.Load())
	for _, g := range got {
		require.Same(t, want, g)
	}
}

func TestLoadMeScopeDoesNotShareFailure(t *testing.T) {
	s := newLoadMeScope()
	var uid proto.UID
	boom := errors.New("boom")
	_, err := s.load(context.Background(), uid, func() (*UserWrapper, error) {
		return nil, boom
	})
	require.ErrorIs(t, err, boom)

	want := &UserWrapper{}
	res, err := s.load(context.Background(), uid, func() (*UserWrapper, error) {
		return want, nil
	})
	require.NoError(t, err)
	require.Same(t, want, res)
}

// A load that panics must not be recorded as a successful nil load: its
// waiters, and later callers, load for themselves.
func TestLoadMeScopePanicIsNotSuccess(t *testing.T) {
	s := newLoadMeScope()
	var uid proto.UID
	started := make(chan struct{})
	waiting := make(chan struct{})
	s.onWait = func() { close(waiting) }

	want := &UserWrapper{}
	waiterRes := make(chan *UserWrapper, 1)
	waiterErr := make(chan error, 1)
	go func() {
		<-started
		res, err := s.load(context.Background(), uid, func() (*UserWrapper, error) {
			return want, nil
		})
		waiterRes <- res
		waiterErr <- err
	}()

	require.Panics(t, func() {
		_, _ = s.load(context.Background(), uid, func() (*UserWrapper, error) {
			close(started)
			<-waiting // the waiter is queued on this flight
			panic("loader blew up")
		})
	}, "the panic reaches the loader's caller")
	require.NoError(t, <-waiterErr)
	require.Same(t, want, <-waiterRes, "the waiter loads for itself")

	called := false
	res, err := s.load(context.Background(), uid, func() (*UserWrapper, error) {
		called = true
		return want, nil
	})
	require.NoError(t, err)
	require.Same(t, want, res)
	// The waiter's own load was not a flight, so this one loads again; what
	// matters is that the panicked flight stored nothing.
	require.True(t, called)
}

func TestLoadMeScopeWaiterHonorsItsContext(t *testing.T) {
	s := newLoadMeScope()
	var uid proto.UID
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = s.load(context.Background(), uid, func() (*UserWrapper, error) {
			close(started)
			<-release
			return &UserWrapper{}, nil
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.load(ctx, uid, func() (*UserWrapper, error) {
		t.Fatal("a waiter must not start its own load")
		return nil, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	close(release)
}

// A waiter shares the flight's failure (one dial per operation, offline),
// but not the loader's own cancellation: then it loads for itself.
func TestLoadMeScopeWaiterSharesFailureNotCancel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		loaderErr error
		shared    bool
	}{
		{"failure", errors.New("offline"), true},
		{"cancel", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLoadMeScope()
			var uid proto.UID
			started := make(chan struct{})
			waiting := make(chan struct{})
			s.onWait = func() { close(waiting) }
			go func() {
				_, _ = s.load(context.Background(), uid, func() (*UserWrapper, error) {
					close(started)
					<-waiting
					return nil, tc.loaderErr
				})
			}()
			<-started
			want := &UserWrapper{}
			ran := false
			res, err := s.load(context.Background(), uid, func() (*UserWrapper, error) {
				ran = true
				return want, nil
			})
			if tc.shared {
				require.ErrorIs(t, err, tc.loaderErr)
				require.False(t, ran)
			} else {
				require.NoError(t, err)
				require.Same(t, want, res)
			}
		})
	}
}

// Work detached from an operation keeps none of its scopes, and is not
// cancelled with it.
func TestDetachContextDropsScopes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx, _ = core.WithRpcStats(ctx)
	ctx, _ = core.WithPrefetchScope(ctx)
	ctx = context.WithValue(ctx, loadMeScopeKey{}, &loadMeScope{})
	type otherKey struct{}
	ctx = context.WithValue(ctx, otherKey{}, "the operation's")
	ctx = ctxlog.WithLogTagWithValue(ctx, "op", "tag")
	ctx, cancelT := context.WithTimeout(ctx, time.Hour)
	defer cancelT()
	cancel()

	d := detachContext(ctx)
	require.NoError(t, d.Err())
	_, hasDeadline := d.Deadline()
	require.False(t, hasDeadline)
	require.Nil(t, d.Value(otherKey{}), "a fresh context: none of the operation's values")
	tags, ok := ctxlog.TagsFromContext(d)
	require.True(t, ok)
	require.Equal(t, "tag", tags["op"], "log tags are kept")
	require.Nil(t, core.RpcStatsFromContext(d))
	require.Nil(t, core.PrefetchScopeFromContext(d))
	s, _ := d.Value(loadMeScopeKey{}).(*loadMeScope)
	require.Nil(t, s)

	// A new operation started from detached work gets a scope of its own.
	_, fresh := core.WithPrefetchScope(d)
	require.NotNil(t, fresh)
}
