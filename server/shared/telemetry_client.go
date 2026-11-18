// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Fork-only (docs/client-analytics.md). The attribute values come only from
// proto enum names (ClientLatencyMetricRevMap, ClientPlatformRevMap) or the
// fixed drop reasons below: a small, fixed set, never anything per user.

func (t *Telemetry) RecordClientLatency(ctx context.Context, metricName, platform string, ms float64) {
	t.inst.clientLatency.Record(ctx, ms, metric.WithAttributes(
		attribute.String("client.metric", metricName),
		attribute.String("client.platform", platform),
	))
}

func (t *Telemetry) ClientLatencyDropped(ctx context.Context, reason string) {
	t.inst.clientLatencyDropped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", reason),
	))
}
