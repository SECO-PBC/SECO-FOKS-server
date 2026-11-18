// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Fork-only (docs/api-sessions.md): instrumentation for the web server's
// /api/v1 surface. Attributes are the chi route pattern (a fixed, small set)
// and the status class; never a uid, token, or raw path.

// StartHTTPAPISpan starts a server span for an /api/v1 request. The span is
// renamed in FinishHTTPAPIRequest, once routing has found the route pattern.
func (t *Telemetry) StartHTTPAPISpan(ctx context.Context, method string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "api",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("http.request.method", method)),
	)
}

// HTTPStatusClass maps a status code to "2xx".."5xx".
func HTTPStatusClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return fmt.Sprintf("%dxx", status/100)
}

// FinishHTTPAPIRequest names and ends span and records the request's latency.
// route is the chi route pattern (e.g. "/api/v1/ping"), or "" when nothing
// matched -- recorded as "unmatched" so a scanner probing random paths cannot
// mint new series.
func (t *Telemetry) FinishHTTPAPIRequest(
	ctx context.Context,
	span trace.Span,
	method, route string,
	status int,
	elapsed time.Duration,
) {
	if route == "" {
		route = "unmatched"
	}
	name := method + " " + route
	class := HTTPStatusClass(status)
	span.SetName(name)
	span.SetAttributes(
		attribute.String("http.route", route),
		attribute.String("resource.name", name),
		attribute.Int("http.response.status_code", status),
	)
	if status >= 500 {
		span.SetStatus(codes.Error, class)
	}
	span.End()
	t.inst.httpAPIDuration.Record(ctx, ms(elapsed), metric.WithAttributes(
		attribute.String("http.request.method", method),
		attribute.String("http.route", route),
		attribute.String("status_class", class),
	))
}
