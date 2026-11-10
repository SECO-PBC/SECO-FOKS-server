// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-snowpack-rpc/rpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// InstrumentProtocol wraps every handler in p with a server span, a latency
// histogram, an in-flight gauge and an error counter, labelled by
// "<Protocol>.<method>". We wrap at registration because the snowpack RPC
// library's own instrumentation/log hooks see an empty method name for V2
// protocols on the server side.
//
// The RPC wire format carries no trace context, so each call is a root span
// within its service; spans are not linked across services.
//
// When telemetry is disabled p is returned unchanged (zero overhead).
func InstrumentProtocol(g *GlobalContext, p rpc.ProtocolV2) rpc.ProtocolV2 {
	t := g.Telemetry()
	if !t.Enabled() {
		return p
	}
	methods := make(map[rpc.Position]rpc.ServeHandlerDescriptionV2, len(p.Methods))
	for pos, d := range p.Methods {
		d.Handler = t.wrapRPC(p.Name+"."+d.Name, d.Handler)
		methods[pos] = d
	}
	p.Methods = methods
	return p
}

// instrumentingRegistrar is the ProtocolRegistrar serveConnection hands to
// each server type: every protocol registered through it is instrumented.
type instrumentingRegistrar struct {
	g   *GlobalContext
	srv interface{ RegisterV2(rpc.ProtocolV2) error }
}

func (r instrumentingRegistrar) RegisterV2(p rpc.ProtocolV2) error {
	return r.srv.RegisterV2(InstrumentProtocol(r.g, p))
}

func (t *Telemetry) wrapRPC(
	method string,
	h func(ctx context.Context, arg interface{}) (interface{}, error),
) func(ctx context.Context, arg interface{}) (interface{}, error) {
	methodAttr := attribute.String("rpc.method", method)
	methodOpt := metric.WithAttributes(methodAttr)
	okOpt := metric.WithAttributes(methodAttr, attribute.String("outcome", "ok"))
	errOutcome := attribute.String("outcome", "error")
	inst := t.inst

	return func(ctx context.Context, arg interface{}) (ret interface{}, err error) {
		start := time.Now()
		inst.rpcActive.Add(ctx, 1, methodOpt)
		ctx, span := t.tracer.Start(ctx, method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				methodAttr,
				attribute.String("rpc.system", "snowpack"),
				// Datadog uses resource.name to group spans in APM.
				attribute.String("resource.name", method),
			),
		)
		// Deferred so a panicking handler still ends its span and
		// decrements the in-flight gauge.
		defer func() {
			ms := float64(time.Since(start)) / float64(time.Millisecond)
			inst.rpcActive.Add(ctx, -1, methodOpt)
			if err == nil {
				inst.rpcDuration.Record(ctx, ms, okOpt)
				span.End()
				return
			}
			code := statusCodeName(err)
			inst.rpcDuration.Record(ctx, ms, metric.WithAttributes(methodAttr, errOutcome))
			inst.rpcErrors.Add(ctx, 1, metric.WithAttributes(methodAttr, attribute.String("error.code", code)))
			// Only the status code: raw error strings can echo user data.
			span.SetAttributes(attribute.String("error.code", code))
			span.SetStatus(codes.Error, code)
			span.End()
		}()
		return h(ctx, arg)
	}
}

// statusCodeName maps an error to its proto status code name, a small
// closed set that is safe to use as a metric label.
func statusCodeName(err error) string {
	st := core.ErrorToStatus(err)
	if s, ok := proto.StatusCodeRevMap[st.Sc]; ok {
		return s
	}
	return "unknown"
}

// RPCConnOpened / RPCConnClosed track open client connections.
func (t *Telemetry) RPCConnOpened(ctx context.Context) { t.inst.rpcConns.Add(ctx, 1) }
func (t *Telemetry) RPCConnClosed(ctx context.Context) { t.inst.rpcConns.Add(ctx, -1) }
