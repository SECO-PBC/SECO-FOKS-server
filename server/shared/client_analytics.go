// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"sync"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"golang.org/x/time/rate"
)

// Fork-only (docs/client-analytics.md): opt-in client analytics, reported over
// the authenticated User protocol and turned into seco.client.* metrics. The
// sender is known (it's an authenticated RPC) but never recorded: metrics are
// tagged only with the metric and platform enums.

const (
	// maxClientLatencySamples bounds one batch. A client flushes about once
	// a minute; this is far above what a minute of honest use produces.
	maxClientLatencySamples = 512

	// maxClientLatency drops absurd values (a suspended app, a wrong clock)
	// instead of letting them skew the distribution.
	maxClientLatency = 10 * time.Minute

	// Per-user batch rate: one every 5s, burst 10. Honest clients send one a
	// minute; this caps a runaway loop without a table or a background job.
	clientAnalyticsEvery = 5 * time.Second
	clientAnalyticsBurst = 10
)

type clientAnalyticsKey struct {
	shid core.ShortHostID
	uid  proto.UID
}

// clientAnalyticsLimiters holds one limiter per user who has ever reported,
// for the life of the process: a few hundred bytes per user, bounded by the
// number of users.
var clientAnalyticsLimiters sync.Map // clientAnalyticsKey -> *rate.Limiter

func clientAnalyticsAllow(m MetaContext) bool {
	k := clientAnalyticsKey{shid: m.ShortHostID(), uid: m.UID()}
	lim, _ := clientAnalyticsLimiters.LoadOrStore(k,
		rate.NewLimiter(rate.Every(clientAnalyticsEvery), clientAnalyticsBurst))
	return lim.(*rate.Limiter).Allow()
}

// RecordClientAnalytics validates a batch and records it. A malformed batch
// (unknown platform, too many samples) is refused whole. A sample naming a
// metric this server doesn't know -- a newer client -- or an out-of-range
// value is dropped and counted, so version skew shows up in the metrics
// instead of failing the client.
func RecordClientAnalytics(m MetaContext, batch proto.ClientAnalyticsBatch) error {
	if m.UID().IsZero() {
		return core.AuthError{}
	}
	platform, ok := proto.ClientPlatformRevMap[batch.Platform]
	if !ok {
		return core.BadArgsError("unknown client platform")
	}
	if len(batch.Latencies) > maxClientLatencySamples {
		return core.BadArgsError("too many latency samples")
	}
	if !clientAnalyticsAllow(m) {
		return core.RateLimitError{}
	}
	t := m.G().Telemetry()
	for _, s := range batch.Latencies {
		metric, ok := proto.ClientLatencyMetricRevMap[s.Metric]
		switch {
		case !ok:
			t.ClientLatencyDropped(m.Ctx(), "unknown_metric")
		case time.Duration(s.Ms)*time.Millisecond > maxClientLatency:
			t.ClientLatencyDropped(m.Ctx(), "out_of_range")
		default:
			t.RecordClientLatency(m.Ctx(), metric, platform, float64(s.Ms))
		}
	}
	return nil
}
