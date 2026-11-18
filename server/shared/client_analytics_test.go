// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func analyticsMeta(h *telemetryHarness, uid proto.UID) MetaContext {
	return NewMetaContextWithUserHost(context.Background(), h.g, UserHostContext{
		HostID: &core.HostID{Short: 1},
		Uid:    uid,
	})
}

func sample(m proto.ClientLatencyMetric, ms uint64) proto.ClientLatencySample {
	return proto.ClientLatencySample{Metric: m, Ms: ms}
}

// Every value seco.client.* can carry, so a test can prove nothing else
// (a uid, a name, free text) ever reaches it.
func clientTagValues() map[string]bool {
	ret := map[string]bool{"unknown": true}
	for m := range proto.ClientLatencyMetricRevMap {
		ret[clientMetricTag(m)] = true
	}
	for p := range proto.ClientPlatformRevMap {
		ret[clientPlatformTag(p)] = true
	}
	for _, r := range []clientDropReason{clientDropInvalid, clientDropUnknownMetric, clientDropOutOfRange} {
		ret[r.tag()] = true
	}
	return ret
}

func assertClientTagsClosed(t *testing.T, rm metricdata.ResourceMetrics, uid proto.UID) {
	allowed := clientTagValues()
	uidHex := hex.EncodeToString(uid[:])
	check := func(set attribute.Set) {
		for _, kv := range set.ToSlice() {
			v := kv.Value.AsString()
			require.Truef(t, allowed[v], "%s=%q is not a fixed client tag", kv.Key, v)
			require.NotContains(t, v, uidHex)
		}
	}
	for _, name := range []string{"seco.client.latency", "seco.client.latency.dropped"} {
		m := findMetric(rm, name)
		if m == nil {
			continue
		}
		for _, set := range metricAttrSets(*m) {
			check(set)
		}
	}
}

func TestRecordClientAnalytics(t *testing.T) {
	h := newTelemetryHarness(t, false)
	uid := proto.UID{0x01, 0xa1, 0x5e, 0xc7}
	m := analyticsMeta(h, uid)

	err := RecordClientAnalytics(m, proto.ClientAnalyticsBatch{
		Platform: proto.ClientPlatform_Ios,
		Latencies: []proto.ClientLatencySample{
			sample(proto.ClientLatencyMetric_SendAck, 120),
			sample(proto.ClientLatencyMetric_SendAck, 80),
			sample(proto.ClientLatencyMetric_ColdStart, proto.MaxClientLatencyMs), // at the cap: kept
			sample(0, 10), // missing metric
			sample(proto.ClientLatencyMetric(99), 10), // a newer client's metric
			sample(proto.ClientLatencyMetric_RecvRender, proto.MaxClientLatencyMs+1),
			// Large enough to overflow a time.Duration conversion; must still
			// be refused, not wrap around and be recorded.
			sample(proto.ClientLatencyMetric_RecvRender, math.MaxUint64),
			sample(proto.ClientLatencyMetric_RecvRender, 9_300_000_000_000),
		},
	})
	require.NoError(t, err)

	rm := h.collect(t)
	lat := findMetric(rm, "seco.client.latency")
	require.NotNil(t, lat)
	counts := map[string]uint64{}
	for _, dp := range lat.Data.(metricdata.Histogram[float64]).DataPoints {
		metric, _ := dp.Attributes.Value("client.metric")
		platform, _ := dp.Attributes.Value("client.platform")
		require.Equal(t, "ios", platform.AsString())
		counts[metric.AsString()] += dp.Count
		max, _ := dp.Max.Value()
		require.LessOrEqual(t, max, float64(proto.MaxClientLatencyMs))
	}
	require.Equal(t, map[string]uint64{"send_ack": 2, "cold_start": 1}, counts)

	dropped := findMetric(rm, "seco.client.latency.dropped")
	require.NotNil(t, dropped)
	reasons := map[string]int64{}
	for _, dp := range dropped.Data.(metricdata.Sum[int64]).DataPoints {
		r, _ := dp.Attributes.Value("client.drop_reason")
		reasons[r.AsString()] += dp.Value
	}
	require.Equal(t, map[string]int64{"invalid": 1, "unknown_metric": 1, "out_of_range": 3}, reasons)

	assertClientTagsClosed(t, rm, uid)
	h.assertPrivate(t)
}

// A platform this server doesn't know (a newer client) is recorded, not
// refused, under the fixed "unknown" tag.
func TestRecordClientAnalyticsUnknownPlatform(t *testing.T) {
	h := newTelemetryHarness(t, false)
	uid := proto.UID{0x01, 0xa2}
	for _, p := range []proto.ClientPlatform{0, 99} {
		err := RecordClientAnalytics(analyticsMeta(h, uid), proto.ClientAnalyticsBatch{
			Platform:  p,
			Latencies: []proto.ClientLatencySample{sample(proto.ClientLatencyMetric_SendAck, 5)},
		})
		require.NoError(t, err)
	}
	rm := h.collect(t)
	lat := findMetric(rm, "seco.client.latency")
	require.NotNil(t, lat)
	for _, dp := range lat.Data.(metricdata.Histogram[float64]).DataPoints {
		p, _ := dp.Attributes.Value("client.platform")
		require.Equal(t, "unknown", p.AsString())
		require.Equal(t, uint64(2), dp.Count)
	}
	assertClientTagsClosed(t, rm, uid)
}

func TestRecordClientAnalyticsRefusals(t *testing.T) {
	h := newTelemetryHarness(t, false)
	m := analyticsMeta(h, proto.UID{0x01, 0xa3})

	tooMany := make([]proto.ClientLatencySample, proto.MaxClientLatencySamples+1)
	for i := range tooMany {
		tooMany[i] = sample(proto.ClientLatencyMetric_SendAck, 1)
	}
	err := RecordClientAnalytics(m, proto.ClientAnalyticsBatch{Platform: proto.ClientPlatform_Desktop, Latencies: tooMany})
	require.IsType(t, core.BadArgsError(""), err)

	// No user, no report.
	err = RecordClientAnalytics(analyticsMeta(h, proto.UID{}), proto.ClientAnalyticsBatch{Platform: proto.ClientPlatform_Desktop})
	require.Equal(t, core.AuthError{}, err)

	// The per-user burst, then refusal; another user is unaffected. The
	// limiter lives on the GlobalContext, so -count=N starts fresh each time.
	ok := proto.ClientAnalyticsBatch{Platform: proto.ClientPlatform_Android}
	for range clientAnalyticsBurst {
		require.NoError(t, RecordClientAnalytics(m, ok))
	}
	require.Equal(t, core.RateLimitError{}, RecordClientAnalytics(m, ok))
	require.NoError(t, RecordClientAnalytics(analyticsMeta(h, proto.UID{0x01, 0xa4}), ok))
}

// Every enum value has its own fixed tag; a new enum member without one would
// be recorded as "unknown" and this fails.
func TestClientTelemetryTags(t *testing.T) {
	seen := map[string]bool{}
	for m, name := range proto.ClientLatencyMetricRevMap {
		tag := clientMetricTag(m)
		require.NotEqualf(t, "unknown", tag, "metric %s has no tag", name)
		require.False(t, seen[tag], "duplicate tag %s", tag)
		require.Equal(t, strings.ToLower(tag), tag)
		seen[tag] = true
	}
	for p, name := range proto.ClientPlatformRevMap {
		require.NotEqualf(t, "unknown", clientPlatformTag(p), "platform %s has no tag", name)
	}
}
