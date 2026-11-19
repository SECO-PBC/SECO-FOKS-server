// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"fmt"
	"sync"
	"time"

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

// clientVersionTag is a client.app_version value. Only clientVersionTags.tag
// makes one, so every value is either "major.minor.patch" from a valid
// version or one of the fixed fallbacks below.
type clientVersionTag string

const (
	clientVersionUnknown clientVersionTag = "unknown" // 0.0.0: the client didn't know
	clientVersionInvalid clientVersionTag = "invalid" // a part over MaxClientAppVersionPart
	clientVersionOther   clientVersionTag = "other"   // over the cap this window
)

const (
	// At most this many distinct versions are tagged per window; the rest
	// are "other". The server can't list valid versions in advance, so this
	// is what stops a modified client from minting Datadog series: series
	// per metric stay under (cap + 3) x platforms per hour, whatever is sent.
	// A handful of versions per platform are live at once.
	clientVersionTagCap    = 32
	clientVersionTagWindow = time.Hour
)

// clientVersionTags is the bounded set of app versions tagged in the current
// window. It resets every window (Datadog counts series per hour), so a
// release made after the set filled up gets its own tag within the hour.
type clientVersionTags struct {
	mu    sync.Mutex
	start time.Time
	seen  map[proto.ClientAppVersion]clientVersionTag
}

func (c *clientVersionTags) tag(v proto.ClientAppVersion, now time.Time) clientVersionTag {
	switch {
	case v.IsZero():
		return clientVersionUnknown
	case !v.IsValid():
		return clientVersionInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil || now.Sub(c.start) >= clientVersionTagWindow || now.Before(c.start) {
		c.seen = make(map[proto.ClientAppVersion]clientVersionTag, clientVersionTagCap)
		c.start = now
	}
	if tag, ok := c.seen[v]; ok {
		return tag
	}
	if len(c.seen) >= clientVersionTagCap {
		return clientVersionOther
	}
	tag := clientVersionTag(fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch))
	c.seen[v] = tag
	return tag
}

func (t *Telemetry) RecordClientLatency(
	ctx context.Context,
	m proto.ClientLatencyMetric,
	p proto.ClientPlatform,
	v clientVersionTag,
	ms uint64,
) {
	t.inst.clientLatency.Record(ctx, float64(ms), metric.WithAttributes(
		attribute.String("client.metric", clientMetricTag(m)),
		attribute.String("client.platform", clientPlatformTag(p)),
		attribute.String("client.app_version", string(v)),
	))
}

func (t *Telemetry) ClientLatencyDropped(ctx context.Context, r clientDropReason) {
	t.inst.clientLatencyDropped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("client.drop_reason", r.tag()),
	))
}
