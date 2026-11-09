// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// pgTracer is a pgx QueryTracer + pgxpool AcquireTracer. We use our own
// instead of a third-party one so that it follows the telemetry privacy rule:
// low-cardinality span names, no connection details, no query parameters, and
// errors reduced to their SQLSTATE (Postgres messages can echo values).
type pgTracer struct {
	t      *Telemetry
	dbType attribute.KeyValue
	accOpt metric.MeasurementOption
}

var _ pgx.QueryTracer = (*pgTracer)(nil)
var _ pgxpool.AcquireTracer = (*pgTracer)(nil)

// InstallPgTracer hooks pool queries and acquires into telemetry. No-op when
// telemetry is disabled.
func (t *Telemetry) InstallPgTracer(cfg *pgxpool.Config, dbType string) {
	if !t.Enabled() || cfg == nil {
		return
	}
	a := attribute.String("db.type", dbType)
	cfg.ConnConfig.Tracer = &pgTracer{t: t, dbType: a, accOpt: metric.WithAttributes(a)}
}

type pgQueryKey struct{}
type pgAcquireKey struct{}

type pgQueryState struct {
	start time.Time
	op    string
	span  trace.Span
}

var knownSQLOps = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"WITH": true, "BEGIN": true, "COMMIT": true, "ROLLBACK": true,
	"SAVEPOINT": true, "RELEASE": true, "LOCK": true, "SET": true,
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true,
}

// sqlOp returns the statement's leading keyword, from a closed set.
func sqlOp(sql string) string {
	s := strings.TrimLeft(sql, " \t\r\n(")
	end := strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	})
	if end >= 0 {
		s = s[:end]
	}
	s = strings.ToUpper(s)
	if knownSQLOps[s] {
		return s
	}
	return "OTHER"
}

func (p *pgTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	st := &pgQueryState{start: time.Now(), op: sqlOp(data.SQL)}
	// Only make DB spans under a sampled parent (an RPC or tx span), so
	// background loops don't produce orphan root spans per query.
	if trace.SpanFromContext(ctx).IsRecording() {
		resource := "pg." + st.op + " " + p.dbType.Value.AsString()
		attrs := []attribute.KeyValue{
			p.dbType,
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.operation.name", st.op),
		}
		if p.t.dbStatement {
			// Parameterized text only; Datadog normalizes it into the
			// resource so slow queries group by statement.
			resource = data.SQL
			attrs = append(attrs, attribute.String("db.query.text", data.SQL))
		}
		attrs = append(attrs, attribute.String("resource.name", resource))
		ctx, st.span = p.t.tracer.Start(ctx, "pg."+st.op,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attrs...),
		)
	}
	return context.WithValue(ctx, pgQueryKey{}, st)
}

func (p *pgTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	st, ok := ctx.Value(pgQueryKey{}).(*pgQueryState)
	if !ok {
		return
	}
	ms := float64(time.Since(st.start)) / float64(time.Millisecond)
	opAttr := attribute.String("db.operation.name", st.op)
	outcome := "ok"
	if data.Err != nil {
		outcome = "error"
		code := pgErrClass(data.Err)
		p.t.inst.dbQueryErrors.Add(ctx, 1, metric.WithAttributes(p.dbType, opAttr, attribute.String("db.response.status_code", code)))
		if st.span != nil {
			st.span.SetAttributes(attribute.String("db.response.status_code", code))
			st.span.SetStatus(codes.Error, code)
		}
	}
	p.t.inst.dbQueryDuration.Record(ctx, ms, metric.WithAttributes(p.dbType, opAttr, attribute.String("outcome", outcome)))
	if st.span != nil {
		st.span.End()
	}
}

// pgErrClass reduces an error to a SQLSTATE or a coarse category.
func pgErrClass(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr):
		return pgErr.Code
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "other"
	}
}

func (p *pgTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, pgAcquireKey{}, time.Now())
}

func (p *pgTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireEndData) {
	start, ok := ctx.Value(pgAcquireKey{}).(time.Time)
	if !ok {
		return
	}
	p.t.inst.dbAcquireDuration.Record(ctx, float64(time.Since(start))/float64(time.Millisecond), p.accOpt)
}

// childSpan starts a span only under a recording parent (an RPC span or one
// of its descendants). Background loops -- e.g. the merkle batcher's RetryTx
// every poll tick -- still record metrics but don't mint a root trace each
// time. Without a parent it returns the parent's no-op span.
func (t *Telemetry) childSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	parent := trace.SpanFromContext(ctx)
	if !parent.IsRecording() {
		return ctx, parent
	}
	return t.tracer.Start(ctx, name, opts...)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// txTelemetry instruments one RetryTx2 call. All methods are safe, and do
// nothing, when telemetry is disabled.
type txTelemetry struct {
	t         *Telemetry
	name      attribute.KeyValue
	parentCtx context.Context // the caller's context; parent of post_commit
	ctx       context.Context
	span      trace.Span
	start     time.Time
}

// StartTx opens the db.tx span. name is the caller's static TxName.
func (t *Telemetry) StartTx(m MetaContext, name TxName) (MetaContext, *txTelemetry) {
	x := &txTelemetry{
		t:         t,
		name:      attribute.String("db.tx.name", string(name)),
		parentCtx: m.Ctx(),
		ctx:       m.Ctx(),
		span:      trace.SpanFromContext(m.Ctx()),
		start:     time.Now(),
	}
	if !t.Enabled() {
		return m, x
	}
	x.ctx, x.span = t.childSpan(m.Ctx(), "db.tx",
		trace.WithAttributes(x.name, attribute.String("resource.name", string(name))),
	)
	return m.WithContext(x.ctx), x
}

// Attempt opens a db.tx.attempt child span for try number i. The returned
// func ends it with result committed, retry or error.
func (x *txTelemetry) Attempt(m MetaContext, i int) (MetaContext, func(retry bool, err error)) {
	if !x.t.Enabled() {
		return m, func(bool, error) {}
	}
	ctx, span := x.t.childSpan(m.Ctx(), "db.tx.attempt",
		trace.WithAttributes(attribute.Int("db.tx.attempt", i)),
	)
	return m.WithContext(ctx), func(retry bool, err error) {
		result := "committed"
		switch {
		case retry:
			result = "retry"
		case err != nil:
			result = "error"
			// SQLSTATE or a coarse class only; never err.Error().
			span.SetStatus(codes.Error, pgErrClass(err))
		}
		span.SetAttributes(attribute.String("result", result))
		span.End()
	}
}

// End closes db.tx and records its duration: first BEGIN through the final
// COMMIT or failure, including retries and backoff, excluding the hook.
func (x *txTelemetry) End(retries int, err error) {
	if !x.t.Enabled() {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
		x.span.SetStatus(codes.Error, "tx failed")
	}
	if retries > 0 {
		x.t.inst.dbTxRetries.Add(x.ctx, int64(retries), metric.WithAttributes(x.name))
		x.span.SetAttributes(attribute.Int("db.tx.retries", retries))
	}
	x.t.inst.dbTxDuration.Record(x.ctx, ms(time.Since(x.start)),
		metric.WithAttributes(x.name, attribute.String("outcome", outcome)))
	x.span.End()
}

// PostCommit times RetryTx2's success hook as db.tx.post_commit, a sibling of
// db.tx under the caller's span. m keeps its log tags; only its context
// changes.
func (x *txTelemetry) PostCommit(m MetaContext) (MetaContext, func()) {
	if !x.t.Enabled() {
		return m, func() {}
	}
	start := time.Now()
	ctx, span := x.t.childSpan(x.parentCtx, "db.tx.post_commit",
		trace.WithAttributes(x.name, attribute.String("resource.name", x.name.Value.AsString())),
	)
	return m.WithContext(ctx), func() {
		x.t.inst.dbTxPostCommit.Record(ctx, ms(time.Since(start)), metric.WithAttributes(x.name))
		span.End()
	}
}
