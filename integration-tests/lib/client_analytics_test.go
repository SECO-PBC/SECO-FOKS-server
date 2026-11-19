// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

// Fork-only (docs/client-analytics.md): the reportClientAnalytics RPC end to
// end. Validation and recording are unit-tested in server/shared.

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func TestReportClientAnalytics(t *testing.T) {
	m := testMetaContext()
	tu := GenerateNewTestUser(t)
	ucli, closeFn := tu.newUserCertAndClient(t, m.Ctx())
	defer closeFn()

	err := ucli.ReportClientAnalytics(m.Ctx(), proto.ClientAnalyticsBatch{
		Platform:   proto.ClientPlatform_Ios,
		AppVersion: proto.ClientAppVersion{Major: 2, Minor: 3, Patch: 3},
		Latencies: []proto.ClientLatencySample{
			{Metric: proto.ClientLatencyMetric_SendAck, Ms: 140},
			{Metric: proto.ClientLatencyMetric_ChannelOpen, Ms: 600},
		},
	})
	require.NoError(t, err)

	// Refusals cross the wire as their status, not as a generic failure.
	tooMany := make([]proto.ClientLatencySample, proto.MaxClientLatencySamples+1)
	for i := range tooMany {
		tooMany[i] = proto.ClientLatencySample{Metric: proto.ClientLatencyMetric_SendAck, Ms: 1}
	}
	err = ucli.ReportClientAnalytics(m.Ctx(), proto.ClientAnalyticsBatch{
		Platform:  proto.ClientPlatform_Ios,
		Latencies: tooMany,
	})
	require.Error(t, err)
	require.IsType(t, core.BadArgsError(""), err)
}
