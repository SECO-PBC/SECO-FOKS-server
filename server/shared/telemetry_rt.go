// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Realtime (chat) hot-path telemetry. Phase names, outcomes and results are
// small closed sets chosen by the caller; never pass user-derived values.

// RTPhase times one phase of a realtime operation (e.g. "lock", "insert",
// "fanout", "wake") as a child span and a foks.rt.phase.duration sample.
// Call the returned func when the phase ends.
func (t *Telemetry) RTPhase(m MetaContext, phase string) (MetaContext, func()) {
	if !t.Enabled() {
		return m, func() {}
	}
	start := time.Now()
	ctx, span := t.childSpan(m.Ctx(), "rt."+phase)
	opt := metric.WithAttributes(attribute.String("phase", phase))
	return m.WithContext(ctx), func() {
		t.inst.rtPhase.Record(ctx, float64(time.Since(start))/float64(time.Millisecond), opt)
		span.End()
	}
}

// RTWake times one wake of parked inbox pollers as the "wake" phase, and
// records how many members it woke. op names the operation that caused it
// (e.g. "send", "read_mark"), so a 1000-member send and a 1-member read mark
// land in separate series instead of one mixed distribution. op must come
// from a small closed set. Call the returned func when the wake ends.
func (t *Telemetry) RTWake(m MetaContext, op string, members int) (MetaContext, func()) {
	if !t.Enabled() {
		return m, func() {}
	}
	start := time.Now()
	opAttr := attribute.String("op", op)
	ctx, span := t.childSpan(m.Ctx(), "rt.wake",
		trace.WithAttributes(opAttr, attribute.Int("rt.fanout.members", members)))
	t.inst.rtFanoutMembers.Record(ctx, int64(members), metric.WithAttributes(opAttr))
	opt := metric.WithAttributes(attribute.String("phase", "wake"), opAttr)
	return m.WithContext(ctx), func() {
		t.inst.rtPhase.Record(ctx, ms(time.Since(start)), opt)
		span.End()
	}
}

// RTPollDone records one rtPollInbox call's total wait and how it ended:
// "bumped_immediate" (never parked), "bumped", "timeout", "cancelled",
// or "error".
func (t *Telemetry) RTPollDone(ctx context.Context, d time.Duration, outcome string) {
	t.inst.rtPollDuration.Record(ctx, float64(d)/float64(time.Millisecond),
		metric.WithAttributes(attribute.String("outcome", outcome)))
}

// RTReconcile counts late-join fan-in checks by which tier answered:
// "memory", "cursor" or "full" (the slow pass that runs queries).
func (t *Telemetry) RTReconcile(ctx context.Context, result string) {
	t.inst.rtReconcile.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
