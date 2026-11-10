// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Telemetry: OpenTelemetry metrics + traces for the FOKS server processes,
// exported over OTLP/gRPC (in staging, to a Datadog Agent sidecar).
//
// PRIVACY RULE. SECO's promise is end-to-end encryption, and the team has
// ruled out client-style telemetry. Server telemetry therefore carries only
// aggregate, non-identifying attributes: server type, RPC method name, status
// code, DB type, SQL operation, static transaction names, coarse outcomes and
// bucketed sizes. NEVER attach UIDs, usernames, FQNs, team / channel / device
// IDs, host names of users, message metadata, SQL parameters or raw error
// strings (which can echo values). See docs/observability.md and the guard in
// telemetry_test.go.

// ServiceVersion is reported as service.version; foks-server's main sets it
// from its linker-stamped version.
var ServiceVersion = "unknown"

const telemetryScope = "github.com/foks-proj/go-foks/server"

type TelemetryConfigJSON struct {
	Enabled bool `json:"enabled"`
	// OTLPEndpoint is host:port of an OTLP/gRPC receiver. Defaults to the
	// Datadog Agent sidecar on the compose network.
	OTLPEndpoint string `json:"otlp_endpoint"`
	// Secure turns on TLS to the OTLP endpoint. Off by default, since the
	// receiver is a sidecar on the private compose network.
	Secure bool `json:"secure"`
	// Env is reported as deployment.environment (Datadog's `env` tag).
	Env string `json:"env"`
	// TraceSampleRatio is the fraction of root spans kept, in [0,1]. Nil
	// means 1.0 (keep everything; fine at staging volumes).
	TraceSampleRatio *float64 `json:"trace_sample_ratio"`
	// TraceSampleMethods overrides TraceSampleRatio per RPC, keyed by
	// "<Protocol>.<method>" (e.g. "RealTime.rtSend"), each a ratio in
	// [0,1]. The whole trace follows its root RPC's decision. Use it to
	// sample down hot methods when traffic is high; see
	// docs/observability.md.
	TraceSampleMethods map[string]float64 `json:"trace_sample_methods"`
	// MetricIntervalMs is the metric export period; default 10s.
	MetricIntervalMs int `json:"metric_interval_ms"`
	// DbStatement records parameterized SQL text on DB spans. Off by
	// default; all server SQL is parameterized, so the text never carries
	// values, but it is opt-in to keep spans lean.
	DbStatement bool `json:"db_statement"`
}

const defaultOTLPEndpoint = "datadog-agent:4317"

// ms converts a duration to float milliseconds, the unit of every foks.*
// duration histogram.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Telemetry holds the process's providers and the pre-built instruments that
// call sites use. A disabled Telemetry is backed by no-op providers, so call
// sites never need nil or enabled checks.
type Telemetry struct {
	enabled     bool
	dbStatement bool
	meter       metric.Meter
	tracer      trace.Tracer
	shutdowns   []func(context.Context) error
	inst        *instruments
}

type instruments struct {
	rpcDuration metric.Float64Histogram
	rpcErrors   metric.Int64Counter
	rpcActive   metric.Int64UpDownCounter
	rpcConns    metric.Int64UpDownCounter

	dbQueryDuration   metric.Float64Histogram
	dbQueryErrors     metric.Int64Counter
	dbAcquireDuration metric.Float64Histogram
	dbTxDuration      metric.Float64Histogram
	dbTxRetries       metric.Int64Counter
	dbTxPostCommit    metric.Float64Histogram

	rtPhase         metric.Float64Histogram
	rtFanoutMembers metric.Int64Histogram
	rtPollDuration  metric.Float64Histogram
	rtReconcile     metric.Int64Counter
}

func newInstruments(mt metric.Meter) (*instruments, error) {
	var errs []error
	f64h := func(name, unit, desc string) metric.Float64Histogram {
		h, err := mt.Float64Histogram(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)
		return h
	}
	i64h := func(name, unit, desc string) metric.Int64Histogram {
		h, err := mt.Int64Histogram(name, metric.WithUnit(unit), metric.WithDescription(desc))
		errs = append(errs, err)
		return h
	}
	ctr := func(name, desc string) metric.Int64Counter {
		c, err := mt.Int64Counter(name, metric.WithDescription(desc))
		errs = append(errs, err)
		return c
	}
	udc := func(name, desc string) metric.Int64UpDownCounter {
		c, err := mt.Int64UpDownCounter(name, metric.WithDescription(desc))
		errs = append(errs, err)
		return c
	}
	ret := &instruments{
		rpcDuration: f64h("foks.rpc.server.duration", "ms", "Server-side RPC handler latency"),
		rpcErrors:   ctr("foks.rpc.server.errors", "RPCs that returned an error, by status code"),
		rpcActive:   udc("foks.rpc.server.active", "RPC handlers currently running"),
		rpcConns:    udc("foks.rpc.server.connections", "Open client RPC connections"),

		dbQueryDuration:   f64h("foks.db.query.duration", "ms", "Postgres query latency (pgx)"),
		dbQueryErrors:     ctr("foks.db.query.errors", "Postgres query errors, by SQLSTATE"),
		dbAcquireDuration: f64h("foks.db.pool.acquire.duration", "ms", "Time waiting to acquire a pooled connection"),
		dbTxDuration:      f64h("foks.db.tx.duration", "ms", "RetryTx transaction latency, first BEGIN to final COMMIT, incl. retries"),
		dbTxRetries:       ctr("foks.db.tx.retries", "RetryTx retries of a COMMIT that never reached the server"),
		dbTxPostCommit:    f64h("foks.db.tx.post_commit.duration", "ms", "RetryTx2 success hook, run after COMMIT"),

		rtPhase:         f64h("foks.rt.phase.duration", "ms", "Realtime write/poll path, per phase"),
		rtFanoutMembers: i64h("foks.rt.fanout.members", "{member}", "Inboxes bumped and woken per write"),
		rtPollDuration:  f64h("foks.rt.poll.duration", "ms", "rtPollInbox wait, by outcome"),
		rtReconcile:     ctr("foks.rt.reconcile", "Late-join fan-in checks, by which tier answered"),
	}
	return ret, errors.Join(errs...)
}

func newNoopTelemetry() *Telemetry {
	mt := metricnoop.NewMeterProvider().Meter(telemetryScope)
	inst, _ := newInstruments(mt)
	return &Telemetry{
		meter:  mt,
		tracer: tracenoop.NewTracerProvider().Tracer(telemetryScope),
		inst:   inst,
	}
}

// newTelemetry builds an enabled Telemetry on the given providers.
func newTelemetry(mp metric.MeterProvider, tp trace.TracerProvider, dbStatement bool) (*Telemetry, error) {
	t := &Telemetry{
		enabled:     true,
		dbStatement: dbStatement,
		meter:       mp.Meter(telemetryScope),
		tracer:      tp.Tracer(telemetryScope),
	}
	var err error
	t.inst, err = newInstruments(t.meter)
	return t, err
}

func (t *Telemetry) Enabled() bool        { return t.enabled }
func (t *Telemetry) Meter() metric.Meter  { return t.meter }
func (t *Telemetry) Tracer() trace.Tracer { return t.tracer }
func (t *Telemetry) DbStatement() bool    { return t.dbStatement }

// Shutdown flushes and stops the exporters. Safe on a no-op Telemetry.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for _, f := range t.shutdowns {
		errs = append(errs, f(ctx))
	}
	return errors.Join(errs...)
}

// telemetryHolder lets hot paths read the current Telemetry without taking
// the GlobalContext lock.
type telemetryHolder struct {
	p atomic.Pointer[Telemetry]
}

func (g *GlobalContext) Telemetry() *Telemetry {
	return g.telemetry.p.Load()
}

// temporality: Datadog wants delta for counters and histograms (so that
// histograms become accurate distributions); up-down counters and gauges
// stay cumulative.
func datadogTemporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	switch k {
	case sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindHistogram,
		sdkmetric.InstrumentKindObservableCounter:
		return metricdata.DeltaTemporality
	default:
		return metricdata.CumulativeTemporality
	}
}

// InitTelemetry builds real providers when telemetry is enabled in config,
// and installs them on the GlobalContext. It must run before the server
// starts accepting connections. Failure to build exporters is returned, but
// callers treat it as non-fatal: a server without telemetry beats no server.
func InitTelemetry(m MetaContext, typ string) error {
	g := m.G()
	cfg := g.Config().TelemetryConfig(m.Ctx())
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	ctx := m.Ctx()

	endpoint := cfg.OTLPEndpoint
	if endpoint == "" {
		endpoint = defaultOTLPEndpoint
	}
	interval := 10 * time.Second
	if cfg.MetricIntervalMs > 0 {
		interval = time.Duration(cfg.MetricIntervalMs) * time.Millisecond
	}
	ratio := 1.0
	if cfg.TraceSampleRatio != nil {
		ratio = *cfg.TraceSampleRatio
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceName("foks-" + typ),
		semconv.ServiceVersion(ServiceVersion),
	}
	if cfg.Env != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentName(cfg.Env))
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		return err
	}

	mopts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithTemporalitySelector(datadogTemporality),
	}
	topts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
	if !cfg.Secure {
		mopts = append(mopts, otlpmetricgrpc.WithInsecure())
		topts = append(topts, otlptracegrpc.WithInsecure())
	}
	mexp, err := otlpmetricgrpc.New(ctx, mopts...)
	if err != nil {
		return err
	}
	texp, err := otlptracegrpc.New(ctx, topts...)
	if err != nil {
		_ = mexp.Shutdown(ctx)
		return err
	}

	// Exponential histograms map to Datadog distributions, which give
	// accurate p50/p99 without us guessing bucket boundaries.
	expView := sdkmetric.NewView(
		sdkmetric.Instrument{Kind: sdkmetric.InstrumentKindHistogram},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{MaxSize: 160, MaxScale: 20}},
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp, sdkmetric.WithInterval(interval))),
		sdkmetric.WithView(expView),
	)
	sampler, problems := newMethodSampler(ratio, cfg.TraceSampleMethods)
	for _, p := range problems {
		m.Warnw("telemetry", "stage", "trace_sample_methods", "skipped", p)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(texp),
		sdktrace.WithSampler(sdktrace.ParentBased(sampler)),
	)
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		g.Log().Warnw("otel", "err", err)
	}))

	t, err := newTelemetry(mp, tp, cfg.DbStatement)
	t.shutdowns = []func(context.Context) error{tp.Shutdown, mp.Shutdown}
	if err != nil {
		_ = t.Shutdown(ctx)
		return err
	}
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		_ = t.Shutdown(ctx)
		return err
	}
	if err := registerObservables(g, t, typ); err != nil {
		_ = t.Shutdown(ctx)
		return err
	}

	g.telemetry.p.Store(t)
	m.Infow("telemetry", "stage", "enabled", "endpoint", endpoint, "env", cfg.Env, "sampler", sampler.Description())
	return nil
}

// ShutdownTelemetry flushes pending metrics and spans. Must be called
// WITHOUT the GlobalContext lock held: a final collection runs observable
// callbacks, which read DB pool state under that lock.
func (g *GlobalContext) ShutdownTelemetry() {
	t := g.Telemetry()
	if t == nil || !t.enabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.Shutdown(ctx); err != nil {
		g.Log().Warnw("telemetry", "stage", "shutdown", "err", err)
	}
	g.telemetry.p.Store(newNoopTelemetry())
}
