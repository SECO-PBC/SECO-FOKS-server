// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"encoding/hex"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

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
	for m := range proto.ClientLatencyMetricRevMap {
		if src, ok := clientSourceTag(m); ok {
			ret[src] = true
		}
	}
	for _, r := range []clientDropReason{clientDropInvalid, clientDropUnknownMetric, clientDropOutOfRange} {
		ret[r.tag()] = true
	}
	for _, v := range []clientVersionTag{clientVersionUnknown, clientVersionInvalid, clientVersionOther} {
		ret[string(v)] = true
	}
	return ret
}

// client.app_version is the one tag that isn't from a fixed list: it may
// also be a dotted version, and nothing else.
var versionTagRE = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)

func assertClientTagsClosed(t *testing.T, rm metricdata.ResourceMetrics, uid proto.UID) {
	allowed := clientTagValues()
	uidHex := hex.EncodeToString(uid[:])
	check := func(set attribute.Set) {
		for _, kv := range set.ToSlice() {
			v := kv.Value.AsString()
			isVersion := kv.Key == "client.app_version" && versionTagRE.MatchString(v)
			require.Truef(t, allowed[v] || isVersion, "%s=%q is not a fixed client tag", kv.Key, v)
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
		Platform:   proto.ClientPlatform_Ios,
		AppVersion: proto.ClientAppVersion{Major: 2, Minor: 3, Patch: 3},
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
		version, _ := dp.Attributes.Value("client.app_version")
		require.Equal(t, "2.3.3", version.AsString())
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

// Channel opens are one metric, split by client.source.
func TestRecordClientAnalyticsChannelOpenSource(t *testing.T) {
	h := newTelemetryHarness(t, false)
	uid := proto.UID{0x01, 0xa5}
	err := RecordClientAnalytics(analyticsMeta(h, uid), proto.ClientAnalyticsBatch{
		Platform:   proto.ClientPlatform_Ios,
		AppVersion: proto.ClientAppVersion{Major: 2, Minor: 4},
		Latencies: []proto.ClientLatencySample{
			sample(proto.ClientLatencyMetric_ChannelOpenMemory, 1),
			sample(proto.ClientLatencyMetric_ChannelOpenWarm, 200),
			sample(proto.ClientLatencyMetric_ChannelOpenWarm, 300),
			sample(proto.ClientLatencyMetric_ChannelOpenCold, 900),
			sample(proto.ClientLatencyMetric_ChannelOpenOffline, 50),
			sample(proto.ClientLatencyMetric_SendAck, 80),
		},
	})
	require.NoError(t, err)
	rm := h.collect(t)
	lat := findMetric(rm, "seco.client.latency")
	require.NotNil(t, lat)
	counts := map[string]uint64{}
	for _, dp := range lat.Data.(metricdata.Histogram[float64]).DataPoints {
		metric, _ := dp.Attributes.Value("client.metric")
		src, hasSrc := dp.Attributes.Value("client.source")
		key := metric.AsString()
		if hasSrc {
			key += "/" + src.AsString()
		}
		counts[key] += dp.Count
	}
	require.Equal(t, map[string]uint64{
		"channel_open/memory":  1,
		"channel_open/warm":    2,
		"channel_open/cold":    1,
		"channel_open/offline": 1,
		"send_ack":             1,
	}, counts)
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
	// Each enum value is its own series: (client.metric, client.source) is
	// unique. Several values may share a metric tag only if each has a
	// source (the channel opens).
	seen := map[[2]string]bool{}
	for m, name := range proto.ClientLatencyMetricRevMap {
		tag := clientMetricTag(m)
		require.NotEqualf(t, "unknown", tag, "metric %s has no tag", name)
		require.Equal(t, strings.ToLower(tag), tag)
		src, _ := clientSourceTag(m)
		key := [2]string{tag, src}
		require.Falsef(t, seen[key], "duplicate series %v", key)
		seen[key] = true
	}
	require.True(t, seen[[2]string{"channel_open", "memory"}])
	for key := range seen {
		if key[1] == "" {
			continue
		}
		require.Falsef(t, seen[[2]string{key[0], ""}],
			"%s has some values with a source and one without", key[0])
	}
	for p, name := range proto.ClientPlatformRevMap {
		require.NotEqualf(t, "unknown", clientPlatformTag(p), "platform %s has no tag", name)
	}
}

func TestClientVersionTags(t *testing.T) {
	var c clientVersionTags
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	v := func(major, minor, patch uint64) proto.ClientAppVersion {
		return proto.ClientAppVersion{Major: major, Minor: minor, Patch: patch}
	}
	max := proto.MaxClientAppVersionPart

	require.Equal(t, clientVersionUnknown, c.tag(v(0, 0, 0), t0))
	require.Equal(t, clientVersionTag("2.3.3"), c.tag(v(2, 3, 3), t0))
	require.Equal(t, clientVersionTag("0.0.1"), c.tag(v(0, 0, 1), t0))
	require.Equal(t, clientVersionTag("9999.9999.9999"), c.tag(v(max, max, max), t0))
	for _, bad := range []proto.ClientAppVersion{v(max+1, 0, 0), v(1, max+1, 0), v(1, 0, max+1), v(math.MaxUint64, 0, 0)} {
		require.Equal(t, clientVersionInvalid, c.tag(bad, t0))
	}

	// Fill to the cap: past it, new versions are "other", known ones keep
	// their tag. Unknown and invalid never take a slot.
	for i := range uint64(clientVersionTagCap) {
		c.tag(v(1, 0, i), t0)
	}
	require.Len(t, c.seen, clientVersionTagCap)
	require.Equal(t, clientVersionOther, c.tag(v(7, 7, 7), t0.Add(time.Minute)))
	require.Equal(t, clientVersionTag("2.3.3"), c.tag(v(2, 3, 3), t0.Add(time.Minute)))

	// A new window starts empty, so a newer release gets its own tag.
	require.Equal(t, clientVersionTag("7.7.7"), c.tag(v(7, 7, 7), t0.Add(clientVersionTagWindow)))
	require.Len(t, c.seen, 1)

	// The clock moving back also starts a new window, not a stuck one.
	require.Equal(t, clientVersionTag("8.0.0"), c.tag(v(8, 0, 0), t0))
	require.Len(t, c.seen, 1)

	for _, tag := range c.seen {
		require.Regexp(t, versionTagRE, string(tag))
	}
}
