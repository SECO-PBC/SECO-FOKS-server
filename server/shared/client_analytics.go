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
// tagged only with fixed strings chosen in telemetry_client.go.

const (
	// Per-user batch rate: one every 5s, burst 10. Honest clients send one a
	// minute; this caps a runaway loop without a table or a background job.
	clientAnalyticsEvery = 5 * time.Second
	clientAnalyticsBurst = 10
)

type clientAnalyticsKey struct {
	shid core.ShortHostID
	uid  proto.UID
}

// clientAnalyticsLimiters holds one limiter per user who has reported, for the
// life of the GlobalContext: a few hundred bytes per user, bounded by the
// number of users.
type clientAnalyticsLimiters struct {
	m sync.Map // clientAnalyticsKey -> *rate.Limiter
}

func (l *clientAnalyticsLimiters) allow(k clientAnalyticsKey) bool {
	lim, ok := l.m.Load(k)
	if !ok {
		lim, _ = l.m.LoadOrStore(k,
			rate.NewLimiter(rate.Every(clientAnalyticsEvery), clientAnalyticsBurst))
	}
	return lim.(*rate.Limiter).Allow()
}

// RecordClientAnalytics validates a batch and records it.
//
// The batch is refused whole only when it's too big or its sender is over
// their rate. Anything a newer client could legitimately send that this
// server doesn't know -- a platform or a metric -- is recorded under a fixed
// "unknown" tag or dropped and counted, so version skew shows up in the
// metrics instead of failing the client.
func RecordClientAnalytics(m MetaContext, batch proto.ClientAnalyticsBatch) error {
	if m.UID().IsZero() {
		return core.AuthError{}
	}
	if len(batch.Latencies) > proto.MaxClientLatencySamples {
		return core.BadArgsError("too many latency samples")
	}
	who := clientAnalyticsKey{shid: m.ShortHostID(), uid: m.UID()}
	if !m.G().clientAnalytics.allow(who) {
		return core.RateLimitError{}
	}
	t := m.G().Telemetry()
	version := m.G().clientVersions.tag(batch.AppVersion, who, m.G().Clock())
	for _, s := range batch.Latencies {
		switch {
		case s.Metric == 0:
			t.ClientLatencyDropped(m.Ctx(), clientDropInvalid)
		case !s.Metric.IsKnown():
			t.ClientLatencyDropped(m.Ctx(), clientDropUnknownMetric)
		case s.Ms > proto.MaxClientLatencyMs:
			t.ClientLatencyDropped(m.Ctx(), clientDropOutOfRange)
		default:
			t.RecordClientLatency(m.Ctx(), s.Metric, batch.Platform, version, s.Ms)
		}
	}
	return nil
}
