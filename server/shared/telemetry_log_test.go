// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"encoding/binary"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func observeLogs(g *GlobalContext) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.DebugLevel)
	g.Lock()
	g.log = zap.New(core).Sugar()
	g.Unlock()
	return logs
}

func lastLogFields(t *testing.T, logs *observer.ObservedLogs) map[string]interface{} {
	all := logs.TakeAll()
	require.NotEmpty(t, all)
	return all[len(all)-1].ContextMap()
}

// Logs written under a sampled span carry Datadog's correlation keys, in the
// format Datadog expects for OTel IDs: the lower 64 bits as a decimal.
func TestLogTraceCorrelation(t *testing.T) {
	g := NewGlobalContext(&GlobalContextOpts{Testing: true})
	logs := observeLogs(g)

	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "RealTime.rtSend")
	defer span.End()
	sc := span.SpanContext()
	tid := sc.TraceID()
	sid := sc.SpanID()
	wantTrace := strconv.FormatUint(binary.BigEndian.Uint64(tid[8:]), 10)
	wantSpan := strconv.FormatUint(binary.BigEndian.Uint64(sid[:]), 10)

	m := NewMetaContextBackground(g).WithContext(ctx)
	for name, log := range map[string]func(){
		"Infow":            func() { m.Infow("hello", "k", "v") },
		"Warnw":            func() { m.Warnw("hello") },
		"Errorw":           func() { m.Errorw("hello") },
		"Debugw":           func() { m.Debugw("hello") },
		"WarnwWithContext": func() { NewMetaContextBackground(g).WarnwWithContext(ctx, "hello") },
	} {
		log()
		f := lastLogFields(t, logs)
		require.Equal(t, wantTrace, f[logKeyDDTraceID], name)
		require.Equal(t, wantSpan, f[logKeyDDSpanID], name)
	}

	// Caller fields and ctxlog tags are kept alongside.
	m.WithLogTag("conn").Infow("hello", "k", "v")
	f := lastLogFields(t, logs)
	require.Equal(t, "v", f["k"])
	require.Contains(t, f, "conn")
	require.Equal(t, wantTrace, f[logKeyDDTraceID])
}

// No span, an unsampled span, or telemetry off: no correlation keys, since
// there is no exported trace to link to.
func TestLogTraceCorrelationAbsent(t *testing.T) {
	g := NewGlobalContext(&GlobalContextOpts{Testing: true})
	logs := observeLogs(g)

	unsampled := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	uctx, uspan := unsampled.Tracer("test").Start(context.Background(), "x")
	defer uspan.End()
	require.True(t, uspan.SpanContext().IsValid())

	nctx, nspan := newNoopTelemetry().tracer.Start(context.Background(), "x")
	defer nspan.End()

	for name, ctx := range map[string]context.Context{
		"none":      context.Background(),
		"unsampled": uctx,
		"noop":      nctx,
	} {
		NewMetaContextBackground(g).WithContext(ctx).Infow("hello")
		f := lastLogFields(t, logs)
		require.NotContains(t, f, logKeyDDTraceID, name)
		require.NotContains(t, f, logKeyDDSpanID, name)
	}
}
