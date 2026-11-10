// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// These tests drive the real retryTx2 loop against a fake connection whose
// COMMIT results are scripted, since the retryable case (a COMMIT that
// never reached the server) can't be produced on demand against Postgres.

// retryableErr is what pgx returns when a statement was never sent.
type retryableErr struct{}

func (retryableErr) Error() string     { return "conn busy" }
func (retryableErr) SafeToRetry() bool { return true }

type fakeTxDB struct {
	commitErrs []error // COMMIT result per attempt; nil past the end
	begins     int
	rollbacks  int
}

func (d *fakeTxDB) Begin(context.Context) (pgx.Tx, error) {
	d.begins++
	return &fakeTx{db: d, attempt: d.begins - 1}, nil
}

type fakeTx struct {
	pgx.Tx  // unused methods panic
	db      *fakeTxDB
	attempt int
	closed  bool
}

func (t *fakeTx) Commit(context.Context) error {
	t.closed = true
	if t.attempt < len(t.db.commitErrs) {
		return t.db.commitErrs[t.attempt]
	}
	return nil
}

func (t *fakeTx) Rollback(context.Context) error {
	if t.closed {
		return pgx.ErrTxClosed
	}
	t.closed = true
	t.db.rollbacks++
	return nil
}

type retryRun struct {
	err    error
	db     *fakeTxDB
	hooks  int
	sleeps []time.Duration
	h      *telemetryHarness
}

// runRetry runs retryTx2 under an RPC-style parent span. The hook, if the
// tx commits, starts a "hook" span so the test can see where it ran.
func runRetry(t *testing.T, commitErrs []error, tryErr error) *retryRun {
	r := &retryRun{db: &fakeTxDB{commitErrs: commitErrs}, h: newTelemetryHarness(t, false)}
	old := retryTxSleep
	retryTxSleep = func(d time.Duration) { r.sleeps = append(r.sleeps, d) }
	t.Cleanup(func() { retryTxSleep = old })

	pctx, parent := r.h.t.tracer.Start(context.Background(), "RealTime.rtSend")
	m := NewMetaContext(pctx, r.h.g)
	r.err = retryTx2(m, r.db, "test.tx", func(m MetaContext, tx pgx.Tx) (func(MetaContext), error) {
		if tryErr != nil {
			return nil, tryErr
		}
		return func(hm MetaContext) {
			r.hooks++
			_, s := r.h.t.tracer.Start(hm.Ctx(), "hook")
			s.End()
		}, nil
	})
	parent.End()
	return r
}

func (r *retryRun) attempts(t *testing.T) map[int64]sdktrace.ReadOnlySpan {
	ret := map[int64]sdktrace.ReadOnlySpan{}
	for _, s := range r.h.spans.Ended() {
		if s.Name() == "db.tx.attempt" {
			ret[spanAttr(s, "db.tx.attempt").AsInt64()] = s
		}
	}
	return ret
}

func (r *retryRun) results(t *testing.T) map[int64]string {
	ret := map[int64]string{}
	for i, s := range r.attempts(t) {
		ret[i] = spanAttr(s, "result").AsString()
	}
	return ret
}

func (r *retryRun) retriesMetric(t *testing.T) int64 {
	m := findMetric(r.h.collect(t), "foks.db.tx.retries")
	if m == nil {
		return 0
	}
	var n int64
	for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
		n += p.Value
	}
	return n
}

func (r *retryRun) span(name string) sdktrace.ReadOnlySpan {
	for _, s := range r.h.spans.Ended() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// The hook runs once, inside db.tx.post_commit, after db.tx has ended.
func (r *retryRun) requireHookAfterTx(t *testing.T) {
	require.Equal(t, 1, r.hooks)
	tx, post, hook := r.span("db.tx"), r.span("db.tx.post_commit"), r.span("hook")
	require.NotNil(t, post)
	require.Equal(t, post.SpanContext().SpanID(), hook.Parent().SpanID())
	require.Equal(t, tx.Parent().SpanID(), post.Parent().SpanID(), "post_commit is a sibling of db.tx")
	require.False(t, tx.EndTime().After(hook.StartTime()), "db.tx must end before the hook runs")
}

func TestRetryTx2CommitsFirstTry(t *testing.T) {
	r := runRetry(t, nil, nil)
	require.NoError(t, r.err)
	require.Equal(t, 1, r.db.begins)
	require.Empty(t, r.sleeps)
	require.Equal(t, map[int64]string{0: "committed"}, r.results(t))
	require.EqualValues(t, 0, r.retriesMetric(t))
	r.requireHookAfterTx(t)
}

func TestRetryTx2RetriesThenCommits(t *testing.T) {
	r := runRetry(t, []error{retryableErr{}}, nil)
	require.NoError(t, r.err)
	require.Equal(t, 2, r.db.begins)
	require.Equal(t, []time.Duration{time.Millisecond}, r.sleeps)
	require.Equal(t, map[int64]string{0: "retry", 1: "committed"}, r.results(t))
	require.EqualValues(t, 1, r.retriesMetric(t))
	// The backoff sits between the attempts, not inside the first one.
	a := r.attempts(t)
	require.False(t, a[0].EndTime().After(a[1].StartTime()))
	r.requireHookAfterTx(t)
}

func TestRetryTx2Exhausted(t *testing.T) {
	errs := make([]error, 5)
	for i := range errs {
		errs[i] = retryableErr{}
	}
	r := runRetry(t, errs, nil)
	require.Equal(t, core.TxRetryError{}, r.err)
	require.Equal(t, 5, r.db.begins)
	require.Zero(t, r.hooks)
	require.Nil(t, r.span("db.tx.post_commit"))
	// Four backoffs between five attempts; none after the last.
	require.Equal(t, []time.Duration{1, 2, 4, 8}, func() []time.Duration {
		var ms []time.Duration
		for _, d := range r.sleeps {
			ms = append(ms, d/time.Millisecond)
		}
		return ms
	}())
	require.Equal(t, map[int64]string{0: "retry", 1: "retry", 2: "retry", 3: "retry", 4: "exhausted"}, r.results(t))
	require.Equal(t, "retries_exhausted", r.attempts(t)[4].Status().Description)
	require.EqualValues(t, 4, r.retriesMetric(t))
}

// tryFn errors are not retried, and roll the transaction back.
func TestRetryTx2TryFnError(t *testing.T) {
	boom := errors.New("boom")
	r := runRetry(t, nil, boom)
	require.ErrorIs(t, r.err, boom)
	require.Equal(t, 1, r.db.begins)
	require.Equal(t, 1, r.db.rollbacks)
	require.Zero(t, r.hooks)
	require.Empty(t, r.sleeps)
	require.Equal(t, map[int64]string{0: "error"}, r.results(t))
	require.Equal(t, "other", r.attempts(t)[0].Status().Description)
}

// Deadlocks (and other server errors at COMMIT) are deliberately not
// retried; see RetryTx2.
func TestRetryTx2DeadlockNotRetried(t *testing.T) {
	r := runRetry(t, []error{&pgconn.PgError{Code: "40P01", Message: "deadlock detected"}}, nil)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, r.err, &pgErr)
	require.Equal(t, "40P01", pgErr.Code)
	require.Equal(t, 1, r.db.begins)
	require.Zero(t, r.hooks)
	require.Empty(t, r.sleeps)
	require.Equal(t, "40P01", r.attempts(t)[0].Status().Description)
}

// With telemetry disabled the loop behaves the same.
func TestRetryTx2TelemetryDisabled(t *testing.T) {
	db := &fakeTxDB{commitErrs: []error{retryableErr{}}}
	old := retryTxSleep
	retryTxSleep = func(time.Duration) {}
	t.Cleanup(func() { retryTxSleep = old })

	g := NewGlobalContext(&GlobalContextOpts{Testing: true})
	require.False(t, g.Telemetry().Enabled())
	hooks := 0
	err := retryTx2(NewMetaContext(context.Background(), g), db, "test.tx",
		func(MetaContext, pgx.Tx) (func(MetaContext), error) {
			return func(MetaContext) { hooks++ }, nil
		})
	require.NoError(t, err)
	require.Equal(t, 2, db.begins)
	require.Equal(t, 1, hooks)
}
