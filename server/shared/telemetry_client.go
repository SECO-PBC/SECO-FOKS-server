// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Fork-only (docs/client-analytics.md). These helpers take typed values, not
// strings, so the only tag values that can ever be emitted are the fixed
// strings in the switches below. The Datadog series names are spelled out
// here rather than taken from the proto's Go identifiers, so renaming an enum
// member can't silently rename a series; TestClientTelemetryTags pins that
// every enum value has a tag.

// clientDropReason is why a client latency sample wasn't recorded.
type clientDropReason int

const (
	clientDropInvalid       clientDropReason = iota + 1 // zero/missing metric
	clientDropUnknownMetric                             // a newer client's metric
	clientDropOutOfRange                                // over MaxClientLatencyMs
)

func (r clientDropReason) tag() string {
	switch r {
	case clientDropInvalid:
		return "invalid"
	case clientDropUnknownMetric:
		return "unknown_metric"
	case clientDropOutOfRange:
		return "out_of_range"
	}
	return "unknown"
}

func clientMetricTag(m proto.ClientLatencyMetric) string {
	switch m {
	case proto.ClientLatencyMetric_SendAck:
		return "send_ack"
	case proto.ClientLatencyMetric_RecvRender:
		return "recv_render"
	case proto.ClientLatencyMetric_DaemonReply:
		return "daemon_reply"
	case proto.ClientLatencyMetric_ColdStart:
		return "cold_start"
	case proto.ClientLatencyMetric_ChannelOpen:
		return "channel_open"
	}
	return "unknown"
}

func clientPlatformTag(p proto.ClientPlatform) string {
	switch p {
	case proto.ClientPlatform_Ios:
		return "ios"
	case proto.ClientPlatform_Android:
		return "android"
	case proto.ClientPlatform_Desktop:
		return "desktop"
	}
	return "unknown"
}

func (t *Telemetry) RecordClientLatency(ctx context.Context, m proto.ClientLatencyMetric, p proto.ClientPlatform, ms uint64) {
	t.inst.clientLatency.Record(ctx, float64(ms), metric.WithAttributes(
		attribute.String("client.metric", clientMetricTag(m)),
		attribute.String("client.platform", clientPlatformTag(p)),
	))
}

func (t *Telemetry) ClientLatencyDropped(ctx context.Context, r clientDropReason) {
	t.inst.clientLatencyDropped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("client.drop_reason", r.tag()),
	))
}
