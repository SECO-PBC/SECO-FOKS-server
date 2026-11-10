// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-snowpack-rpc/rpc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// allowedTelemetryKeys is the complete set of attribute keys server telemetry
// may emit. Adding a key here is a privacy decision: it must never carry a
// user, team, channel or device identifier, or any message metadata.
var allowedTelemetryKeys = map[attribute.Key]bool{
	"rpc.method":              true,
	"rpc.system":              true,
	"resource.name":           true,
	"outcome":                 true,
	"error.code":              true,
	"db.type":                 true,
	"db.kv_shard":             true,
	"db.system.name":          true,
	"db.operation.name":       true,
	"db.response.status_code": true,
	"db.query.text":           true,
	"db.tx.name":              true,
	"db.tx.retries":           true,
	"db.tx.attempt":           true,
	"phase":                   true,
	"op":                      true,
	"status":                  true,
	"result":                  true,
	"rt.fanout.members":       true,
}

// A stand-in for user data that must never reach telemetry.
const secretMarker = "SECRET-UID-7f3a9c"

type telemetryHarness struct {
	g      *GlobalContext
	t      *Telemetry
	reader *sdkmetric.ManualReader
	spans  *tracetest.SpanRecorder
}

func newTelemetryHarness(t *testing.T, dbStatement bool) *telemetryHarness {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	tel, err := newTelemetry(mp, tp, dbStatement)
	require.NoError(t, err)
	g := NewGlobalContext(&GlobalContextOpts{Testing: true})
	g.telemetry.p.Store(tel)
	return &telemetryHarness{g: g, t: tel, reader: reader, spans: rec}
}

func (h *telemetryHarness) collect(t *testing.T) metricdata.ResourceMetrics {
	var rm metricdata.ResourceMetrics
	require.NoError(t, h.reader.Collect(context.Background(), &rm))
	return rm
}

func findMetric(rm metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	for _, sm := range rm.ScopeMetrics {
		for i := range sm.Metrics {
			if sm.Metrics[i].Name == name {
				return &sm.Metrics[i]
			}
		}
	}
	return nil
}

// metricAttrSets returns every data point's attribute set, for any metric type.
func metricAttrSets(m metricdata.Metrics) []attribute.Set {
	var ret []attribute.Set
	switch d := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			ret = append(ret, p.Attributes)
		}
	case metricdata.Histogram[int64]:
		for _, p := range d.DataPoints {
			ret = append(ret, p.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			ret = append(ret, p.Attributes)
		}
	case metricdata.Sum[float64]:
		for _, p := range d.DataPoints {
			ret = append(ret, p.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, p := range d.DataPoints {
			ret = append(ret, p.Attributes)
		}
	}
	return ret
}

// assertPrivate checks every exported metric and span against the key
// allowlist, and that the secret marker appears nowhere.
func (h *telemetryHarness) assertPrivate(t *testing.T) {
	rm := h.collect(t)
	check := func(where string, kv attribute.KeyValue) {
		require.Truef(t, allowedTelemetryKeys[kv.Key], "%s: attribute key %q not in allowlist", where, kv.Key)
		require.NotContainsf(t, kv.Value.String(), secretMarker, "%s: %q leaks user data", where, kv.Key)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, set := range metricAttrSets(m) {
				for _, kv := range set.ToSlice() {
					check("metric "+m.Name, kv)
				}
			}
		}
	}
	for _, s := range h.spans.Ended() {
		for _, kv := range s.Attributes() {
			check("span "+s.Name(), kv)
		}
		require.NotContains(t, s.Name(), secretMarker)
		require.NotContains(t, s.Status().Description, secretMarker)
		for _, ev := range s.Events() {
			require.NotContains(t, ev.Name, secretMarker)
			for _, kv := range ev.Attributes {
				require.NotContains(t, kv.Value.String(), secretMarker)
			}
		}
	}
}

func testProtocol(okH, errH func(ctx context.Context, arg interface{}) (interface{}, error)) rpc.ProtocolV2 {
	return rpc.ProtocolV2{
		Name: "TestProto",
		Methods: map[rpc.Position]rpc.ServeHandlerDescriptionV2{
			0: {Name: "good", ServeHandlerDescription: rpc.ServeHandlerDescription{Handler: okH}},
			1: {Name: "bad", ServeHandlerDescription: rpc.ServeHandlerDescription{Handler: errH}},
		},
	}
}

func TestInstrumentProtocol(t *testing.T) {
	h := newTelemetryHarness(t, false)
	okH := func(ctx context.Context, arg interface{}) (interface{}, error) { return "ok", nil }
	errH := func(ctx context.Context, arg interface{}) (interface{}, error) {
		return nil, core.NotFoundError("user " + secretMarker)
	}
	p := InstrumentProtocol(h.g, testProtocol(okH, errH))

	for range 3 {
		ret, err := p.Methods[0].Handler(context.Background(), secretMarker)
		require.NoError(t, err)
		require.Equal(t, "ok", ret)
	}
	_, err := p.Methods[1].Handler(context.Background(), secretMarker)
	require.Error(t, err)

	rm := h.collect(t)

	dur := findMetric(rm, "foks.rpc.server.duration")
	require.NotNil(t, dur)
	counts := map[string]uint64{}
	for _, dp := range dur.Data.(metricdata.Histogram[float64]).DataPoints {
		m, _ := dp.Attributes.Value("rpc.method")
		o, _ := dp.Attributes.Value("outcome")
		counts[m.AsString()+"/"+o.AsString()] += dp.Count
	}
	require.Equal(t, map[string]uint64{"TestProto.good/ok": 3, "TestProto.bad/error": 1}, counts)

	errs := findMetric(rm, "foks.rpc.server.errors")
	require.NotNil(t, errs)
	dps := errs.Data.(metricdata.Sum[int64]).DataPoints
	require.Len(t, dps, 1)
	code, _ := dps[0].Attributes.Value("error.code")
	require.NotEqual(t, "unknown", code.AsString())
	require.NotEmpty(t, code.AsString())

	// In-flight gauge returns to zero.
	active := findMetric(rm, "foks.rpc.server.active")
	require.NotNil(t, active)
	for _, dp := range active.Data.(metricdata.Sum[int64]).DataPoints {
		require.Zero(t, dp.Value)
	}

	ended := h.spans.Ended()
	require.Len(t, ended, 4)
	require.Equal(t, "TestProto.bad", ended[3].Name())

	h.assertPrivate(t)
}

func TestInstrumentProtocolDisabledIsIdentity(t *testing.T) {
	g := NewGlobalContext(&GlobalContextOpts{Testing: true})
	require.False(t, g.Telemetry().Enabled())
	called := false
	okH := func(ctx context.Context, arg interface{}) (interface{}, error) { called = true; return nil, nil }
	p := InstrumentProtocol(g, testProtocol(okH, okH))
	_, _ = p.Methods[0].Handler(context.Background(), nil)
	require.True(t, called)
}

func TestSQLOp(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":                             "SELECT",
		"\n\t  insert into x values ($1)":      "INSERT",
		"(SELECT a FROM b) UNION SELECT 1":     "SELECT",
		"WITH t AS (SELECT 1) SELECT * FROM t": "WITH",
		"VACUUM":                               "OTHER",
		"":                                     "OTHER",
	} {
		require.Equal(t, want, sqlOp(in), in)
	}
}

func TestPgTracer(t *testing.T) {
	h := newTelemetryHarness(t, false)
	cfg := &pgxpool.Config{ConnConfig: &pgx.ConnConfig{}}
	h.t.InstallPgTracer(cfg, DbTypeRealTime.ToString())
	tr := cfg.ConnConfig.Tracer.(*pgTracer)

	// A parent span, as an RPC would provide.
	ctx, parent := h.t.tracer.Start(context.Background(), "parent")

	q := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT * FROM users WHERE uid=$1", Args: []any{secretMarker}})
	tr.TraceQueryEnd(q, nil, pgx.TraceQueryEndData{})

	q = tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "INSERT INTO t VALUES ($1)", Args: []any{secretMarker}})
	tr.TraceQueryEnd(q, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{
		Code:    "23505",
		Message: `duplicate key value violates unique constraint, uid=` + secretMarker,
	}})

	a := tr.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{})
	time.Sleep(time.Millisecond)
	tr.TraceAcquireEnd(a, nil, pgxpool.TraceAcquireEndData{})
	parent.End()

	rm := h.collect(t)
	qd := findMetric(rm, "foks.db.query.duration")
	require.NotNil(t, qd)
	require.Len(t, qd.Data.(metricdata.Histogram[float64]).DataPoints, 2)

	qe := findMetric(rm, "foks.db.query.errors")
	require.NotNil(t, qe)
	dp := qe.Data.(metricdata.Sum[int64]).DataPoints[0]
	code, _ := dp.Attributes.Value("db.response.status_code")
	require.Equal(t, "23505", code.AsString())

	require.NotNil(t, findMetric(rm, "foks.db.pool.acquire.duration"))

	var names []string
	for _, s := range h.spans.Ended() {
		names = append(names, s.Name())
		// db_statement is off: no SQL text anywhere on the span.
		for _, kv := range s.Attributes() {
			require.NotContains(t, kv.Value.String(), "FROM users")
		}
	}
	require.ElementsMatch(t, []string{"pg.SELECT", "pg.INSERT", "parent"}, names)

	h.assertPrivate(t)
}

func TestPgTracerNoParentNoSpan(t *testing.T) {
	h := newTelemetryHarness(t, false)
	cfg := &pgxpool.Config{ConnConfig: &pgx.ConnConfig{}}
	h.t.InstallPgTracer(cfg, DbTypeUsers.ToString())
	tr := cfg.ConnConfig.Tracer.(*pgTracer)
	q := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tr.TraceQueryEnd(q, nil, pgx.TraceQueryEndData{})
	require.Empty(t, h.spans.Ended())
	require.NotNil(t, findMetric(h.collect(t), "foks.db.query.duration"))
}

// spanByName returns the single ended span with this name.
func spanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	var ret []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			ret = append(ret, s)
		}
	}
	require.Lenf(t, ret, 1, "span %q", name)
	return ret[0]
}

func spanAttr(s sdktrace.ReadOnlySpan, k attribute.Key) attribute.Value {
	for _, kv := range s.Attributes() {
		if kv.Key == k {
			return kv.Value
		}
	}
	return attribute.Value{}
}

func TestTxTelemetryAndRTHelpers(t *testing.T) {
	h := newTelemetryHarness(t, false)

	// Without a parent span: metrics only, no root spans.
	bg := NewMetaContext(context.Background(), h.g)
	bm, bgTx := h.t.StartTx(bg, "merkle.poll")
	_, bgAttempt := bgTx.Attempt(bm, 0)
	bgAttempt(false, nil)
	bgTx.End(0, nil)
	_, bgHook := bgTx.PostCommit(bm)
	bgHook()
	_, bgWake := h.t.RTWake(bg, "send", 5)
	bgWake()
	require.Empty(t, h.spans.Ended())

	// Under an RPC-style parent, replay what RetryTx2 does for a send whose
	// first attempt hits a serialization failure.
	pctx, parent := h.t.tracer.Start(context.Background(), "RealTime.rtSend")
	m := NewMetaContext(pctx, h.g)

	tm, tx := h.t.StartTx(m, "realtime.SendMessage")

	am, attemptDone := tx.Attempt(tm, 0)
	_, ph := h.t.RTPhase(am, "fanout")
	ph()
	attemptDone(true, nil) // retried

	am, attemptDone = tx.Attempt(tm, 1)
	_, ph = h.t.RTPhase(am, "fanout")
	ph()
	attemptDone(false, nil) // committed

	tx.End(1, nil)

	hm, hookDone := tx.PostCommit(tm)
	_, wake := h.t.RTWake(hm, "send", 42)
	wake()
	hookDone()

	// A failed transaction: the attempt's status carries only the SQLSTATE.
	fm, ftx := h.t.StartTx(m, "realtime.MarkRead")
	_, fDone := ftx.Attempt(fm, 0)
	pgErr := &pgconn.PgError{Code: "23505", Message: "dup " + secretMarker}
	fDone(false, pgErr)
	ftx.End(0, pgErr)

	h.t.RTPollDone(m.Ctx(), 5*time.Millisecond, "timeout")
	h.t.RTReconcile(m.Ctx(), "memory")
	parent.End()

	rm := h.collect(t)
	for _, n := range []string{
		"foks.db.tx.duration", "foks.db.tx.retries", "foks.db.tx.post_commit.duration",
		"foks.rt.phase.duration", "foks.rt.fanout.members",
		"foks.rt.poll.duration", "foks.rt.reconcile",
	} {
		require.NotNil(t, findMetric(rm, n), n)
	}
	retries := findMetric(rm, "foks.db.tx.retries").Data.(metricdata.Sum[int64]).DataPoints[0]
	require.EqualValues(t, 1, retries.Value)

	ended := h.spans.Ended()
	rpc := spanByName(t, ended, "RealTime.rtSend")
	var sendTx sdktrace.ReadOnlySpan
	for _, s := range ended {
		if s.Name() == "db.tx" && spanAttr(s, "db.tx.name").AsString() == "realtime.SendMessage" {
			sendTx = s
		}
	}
	require.NotNil(t, sendTx)
	post := spanByName(t, ended, "db.tx.post_commit")
	wakeSpan := spanByName(t, ended, "rt.wake")
	require.Equal(t, "send", spanAttr(wakeSpan, "op").AsString())
	require.EqualValues(t, 42, spanAttr(wakeSpan, "rt.fanout.members").AsInt64())

	// db.tx and db.tx.post_commit are siblings under the RPC; the wake is
	// under post_commit, not inside the transaction.
	require.Equal(t, rpc.SpanContext().SpanID(), sendTx.Parent().SpanID())
	require.Equal(t, rpc.SpanContext().SpanID(), post.Parent().SpanID())
	require.Equal(t, post.SpanContext().SpanID(), wakeSpan.Parent().SpanID())
	require.False(t, sendTx.EndTime().After(post.StartTime()), "db.tx must end before the hook starts")
	require.EqualValues(t, 1, spanAttr(sendTx, "db.tx.retries").AsInt64())

	// Two attempts under the send tx, with their results; each attempt
	// holds its own phase span.
	results := map[int64]string{}
	for _, s := range ended {
		if s.Name() == "db.tx.attempt" && s.Parent().SpanID() == sendTx.SpanContext().SpanID() {
			results[spanAttr(s, "db.tx.attempt").AsInt64()] = spanAttr(s, "result").AsString()
		}
	}
	require.Equal(t, map[int64]string{0: "retry", 1: "committed"}, results)

	var failed sdktrace.ReadOnlySpan
	for _, s := range ended {
		if s.Name() == "db.tx.attempt" && spanAttr(s, "result").AsString() == "error" {
			failed = s
		}
	}
	require.NotNil(t, failed)
	require.Equal(t, "23505", failed.Status().Description)

	h.assertPrivate(t)
}

func TestNoopTelemetryIsSafe(t *testing.T) {
	g := NewGlobalContext(&GlobalContextOpts{Testing: true})
	tel := g.Telemetry()
	m := NewMetaContext(context.Background(), g)
	tm, tx := tel.StartTx(m, "x")
	am, attemptDone := tx.Attempt(tm, 0)
	require.Equal(t, m.Ctx(), am.Ctx())
	attemptDone(false, nil)
	tx.End(0, nil)
	_, hookDone := tx.PostCommit(tm)
	hookDone()
	_, pdone := tel.RTPhase(m, "lock")
	pdone()
	_, wdone := tel.RTWake(m, "send", 3)
	wdone()
	tel.RTPollDone(m.Ctx(), time.Millisecond, "bumped")
	tel.RTReconcile(m.Ctx(), "full")
	tel.RPCConnOpened(m.Ctx())
	tel.RPCConnClosed(m.Ctx())
	cfg := &pgxpool.Config{ConnConfig: &pgx.ConnConfig{}}
	tel.InstallPgTracer(cfg, "x")
	require.Nil(t, cfg.ConnConfig.Tracer)
	g.ShutdownTelemetry()
	require.False(t, g.Telemetry().Enabled())
}

func TestMethodSampler(t *testing.T) {
	s, problems := newMethodSampler(0, map[string]float64{
		"RealTime.rtSend":  1.0,
		"RealTime.rtGone":  0,
		"not-a-method":     1.0,
		"RealTime.rtWrong": 2,
	})
	require.Len(t, problems, 2)
	require.Contains(t, problems[0], "RealTime.rtWrong")
	require.Contains(t, problems[1], "not-a-method")

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(rec),
		sdktrace.WithSampler(sdktrace.ParentBased(s)),
	)
	tr := tp.Tracer("test")

	// Overridden to 1.0: the root and its child are both recorded.
	ctx, root := tr.Start(context.Background(), "RealTime.rtSend")
	require.True(t, root.IsRecording())
	_, child := tr.Start(ctx, "pg.SELECT")
	require.True(t, child.IsRecording())
	child.End()
	root.End()

	// Default 0, and an explicit 0 override: neither root is recorded, and a
	// child follows its root even though "pg.SELECT" has no override.
	for _, name := range []string{"RealTime.rtPollInbox", "RealTime.rtGone"} {
		ctx, root := tr.Start(context.Background(), name)
		require.False(t, root.IsRecording(), name)
		_, child := tr.Start(ctx, "pg.SELECT")
		require.False(t, child.IsRecording(), name)
		child.End()
		root.End()
	}
	require.Len(t, rec.Ended(), 2)

	// No overrides: identical to the plain ratio sampler.
	all, problems := newMethodSampler(1.0, nil)
	require.Empty(t, problems)
	_, sp := sdktrace.NewTracerProvider(sdktrace.WithSampler(all)).Tracer("t").Start(context.Background(), "User.anything")
	require.True(t, sp.IsRecording())
}

// Telemetry's own queries (the push_outbox probe) are marked so the pgx
// tracer records nothing for them: no metrics and no spans, even under a
// recording parent.
func TestPgTracerSkipsMarkedContext(t *testing.T) {
	h := newTelemetryHarness(t, false)
	cfg := &pgxpool.Config{ConnConfig: &pgx.ConnConfig{}}
	h.t.InstallPgTracer(cfg, DbTypeRealTime.ToString())
	tr := cfg.ConnConfig.Tracer.(*pgTracer)

	pctx, parent := h.t.tracer.Start(context.Background(), "parent")
	ctx := withoutPgTelemetry(pctx)

	q := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	require.Equal(t, ctx, q)
	tr.TraceQueryEnd(q, nil, pgx.TraceQueryEndData{})
	a := tr.TraceAcquireStart(ctx, nil, pgxpool.TraceAcquireStartData{})
	tr.TraceAcquireEnd(a, nil, pgxpool.TraceAcquireEndData{})
	parent.End()

	rm := h.collect(t)
	require.Nil(t, findMetric(rm, "foks.db.query.duration"))
	require.Nil(t, findMetric(rm, "foks.db.pool.acquire.duration"))
	require.Len(t, h.spans.Ended(), 1, "only the parent span")
}
