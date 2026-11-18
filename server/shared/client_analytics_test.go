// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"context"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func analyticsMeta(h *telemetryHarness, uid proto.UID) MetaContext {
	return NewMetaContextWithUserHost(context.Background(), h.g, UserHostContext{
		HostID: &core.HostID{Short: 1},
		Uid:    uid,
	})
}

func TestRecordClientAnalytics(t *testing.T) {
	h := newTelemetryHarness(t, false)
	m := analyticsMeta(h, proto.UID{0x01, 0xa1})

	err := RecordClientAnalytics(m, proto.ClientAnalyticsBatch{
		Platform: proto.ClientPlatform_Ios,
		Latencies: []proto.ClientLatencySample{
			{Metric: proto.ClientLatencyMetric_SendAck, Ms: 120},
			{Metric: proto.ClientLatencyMetric_SendAck, Ms: 80},
			{Metric: proto.ClientLatencyMetric_ColdStart, Ms: 1500},
			{Metric: proto.ClientLatencyMetric(99), Ms: 10},                                                 // newer client
			{Metric: proto.ClientLatencyMetric_RecvRender, Ms: uint64(maxClientLatency.Milliseconds()) + 1}, // absurd
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
		require.Equal(t, "Ios", platform.AsString())
		counts[metric.AsString()] += dp.Count
	}
	require.Equal(t, map[string]uint64{"SendAck": 2, "ColdStart": 1}, counts)

	dropped := findMetric(rm, "seco.client.latency.dropped")
	require.NotNil(t, dropped)
	reasons := map[string]int64{}
	for _, dp := range dropped.Data.(metricdata.Sum[int64]).DataPoints {
		r, _ := dp.Attributes.Value("reason")
		reasons[r.AsString()] += dp.Value
	}
	require.Equal(t, map[string]int64{"unknown_metric": 1, "out_of_range": 1}, reasons)

	h.assertPrivate(t)
}

func TestRecordClientAnalyticsRefusals(t *testing.T) {
	h := newTelemetryHarness(t, false)
	m := analyticsMeta(h, proto.UID{0x01, 0xa2})

	// Unset or unknown platform: the zero value is not a platform.
	for _, p := range []proto.ClientPlatform{0, 99} {
		err := RecordClientAnalytics(m, proto.ClientAnalyticsBatch{Platform: p})
		require.Error(t, err)
	}

	tooMany := make([]proto.ClientLatencySample, maxClientLatencySamples+1)
	for i := range tooMany {
		tooMany[i] = proto.ClientLatencySample{Metric: proto.ClientLatencyMetric_SendAck, Ms: 1}
	}
	err := RecordClientAnalytics(m, proto.ClientAnalyticsBatch{Platform: proto.ClientPlatform_Desktop, Latencies: tooMany})
	require.Error(t, err)

	// No user, no report.
	err = RecordClientAnalytics(analyticsMeta(h, proto.UID{}), proto.ClientAnalyticsBatch{Platform: proto.ClientPlatform_Desktop})
	require.Equal(t, core.AuthError{}, err)

	// The per-user burst, then refusal; another user is unaffected.
	ok := proto.ClientAnalyticsBatch{Platform: proto.ClientPlatform_Android}
	for range clientAnalyticsBurst {
		require.NoError(t, RecordClientAnalytics(m, ok))
	}
	require.Equal(t, core.RateLimitError{}, RecordClientAnalytics(m, ok))
	require.NoError(t, RecordClientAnalytics(analyticsMeta(h, proto.UID{0x01, 0xa3}), ok))
}
