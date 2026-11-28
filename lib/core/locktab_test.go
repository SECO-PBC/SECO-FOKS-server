// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLocktabAcquireCtx: a wait for a held entry gives up when its context
// is done, holds nothing afterwards, and leaves the table clean once the
// holder releases.
func TestLocktabAcquireCtx(t *testing.T) {
	var lt Locktab[int]
	held := lt.Acquire(1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got, err := lt.AcquireCtx(ctx, 1)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, got)

	// Another key is independent.
	other, err := lt.AcquireCtx(context.Background(), 2)
	require.NoError(t, err)
	other.Release()

	// The abandoned wait did not take the lock: the holder's release lets a
	// new acquirer straight in, and the table is empty after both.
	done := make(chan struct{})
	go func() {
		e, err := lt.AcquireCtx(context.Background(), 1)
		if err == nil {
			e.Release()
		}
		close(done)
	}()
	held.Release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("acquirer never got the released lock")
	}
	lt.Lock()
	defer lt.Unlock()
	require.Empty(t, lt.m)
}
