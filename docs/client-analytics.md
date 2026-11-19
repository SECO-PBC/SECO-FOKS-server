# Client analytics (fork-only)

Opt-in analytics from the SECO app, starting with end-to-end latency. The app
batches samples and reports them over the authenticated User protocol; the
user server turns them into `seco.client.*` metrics.

## Wire format

`proto-src/lib/analytics.snowp`:

- `ClientAnalyticsBatch{platform, latencies, appVersion}`, where each latency
  is `{metric, ms}` and `appVersion` is `{major, minor, patch}` (the release,
  e.g. 2.3.3; mobile and desktop are versioned separately).
- `ClientLatencyMetric` and `ClientPlatform` are **enums**, numbered from 1 so
  the zero value is never valid. A metric the server doesn't know can't be
  sent as a name; the only way to add one is a proto change.
- rem `User.reportClientAnalytics @201`. The SECO bridge calls it directly on
  the active user's User client; there is no agent-protocol (lcl) method.
- The batch limits live in `proto/lib/analytics_extras.go`
  (`MaxClientLatencySamples`, `MaxClientLatencyMs`,
  `MaxClientAppVersionPart`), shared with the bridge,
  which splits and filters batches so a valid client never hits them.

## What the server does (`shared.RecordClientAnalytics`)

- Refuses the whole batch only when it has more than `MaxClientLatencySamples`
  samples or its sender is over the per-user rate (one batch per 5s, burst
  10, in memory on the GlobalContext). Both answers are marked expected in
  the RPC telemetry, so they don't count as server errors.
- A platform it doesn't know (a newer client, or unset) is recorded under the
  `unknown` platform tag rather than refused.
- Drops and counts individual samples with no metric (`invalid`), a metric it
  doesn't know (`unknown_metric`, a newer client) or a value over
  `MaxClientLatencyMs` (`out_of_range`), compared as integers.
- Records the rest in `seco.client.latency` (milliseconds), tagged only with
  `client.metric`, `client.platform` and `client.app_version`, plus
  `client.source` on channel opens.

Channel opens are four enum values (`ChannelOpenMemory`, `Warm`, `Cold`,
`Offline`) recorded as one `client.metric:channel_open` with a fixed
`client.source` tag (`memory`, `warm`, `cold`, `offline`). Four enum values
rather than a source field mean a source can't be attached to a metric it
doesn't apply to. The source comes from the app (`memory`: it already held the
messages) or from the agent's thread read (`lcl.RTThreadView.readSource`).
The head page of a thread always calls the server for anything newer, so
`warm` means "one round trip, the cache supplied the rest", not "no server
call".

`client.app_version` is the one tag the server can't list in advance, so it
is bounded instead (`clientVersionTags`): `major.minor.patch` for at most 32
distinct versions per hour per server, then `other`, and no user may introduce
more than two of them per hour, so one client can't fill the set; `unknown` for 0.0.0 (a
client that didn't know its version) and `invalid` for a part over
`MaxClientAppVersionPart`. A modified client can't add Datadog series beyond
that cap. The set resets every hour, so a new release gets its own tag even if
the set filled up.

The tag strings (`send_ack`, `ios`, ...) are spelled out in
`telemetry_client.go`, not taken from the proto's Go identifiers, so renaming
an enum member can't silently rename a Datadog series. Its helpers take the
proto enums and a typed drop reason, never strings, and
`TestClientTelemetryTags` fails if an enum member has no tag.

## Privacy

The RPC is authenticated, so the server knows who sent each batch, but none
of it is recorded: no uid, team, channel or device in any metric, span or
log. Tag values come only from enum names, plus the app version, which every
user of a release shares. Never add a dimension that could
identify a user, team, channel or device, and never put free text in a batch.

Opting in is a client setting; the server can't tell an opted-in client from
any other and doesn't try.

## Adding a metric

1. Add it to `ClientLatencyMetric` (or a new enum, for a new kind of
   measurement), regenerate, and give it a tag in `clientMetricTag`.
2. Tag a fork release and bump the pin in SECO-FOKS, then run `go generate`
   in its `foks-gomobile` to regenerate the app's TS types from the proto.
3. Deploy the server before shipping a client that sends it; until then the
   server drops it as `unknown_metric`. A new platform is recorded as
   `unknown` until then.

## Why RPC and not HTTP

An authenticated HTTPS endpoint was built and reviewed first (SECO-FOKS-server
PR #110). RPC was chosen for now because it needs no second auth system (no
tokens, keys or verifier), revocation is automatic, there's no new public
endpoint, and far less code is ours to own.

Revisit HTTP when one of these becomes true:

- analytics ingest should run as its own service, outside this fork and the
  user server process;
- request-level load balancing or edge infrastructure is needed for ingest;
- analytics should be anonymous to the server, not just unrecorded;
- events must be reported when the agent is down, before login, or from a
  client with no agent;
- users on non-SECO FOKS hosts need to report to SECO.

The client keeps transport out of its collector, so switching swaps only how
batches are sent.

## Future: client spans

Investigate adding proper client-side spans. Today the app sends only
latency histograms. They show *that* a send, a chat open or a cold start was
slow, but not *where* the time went. A follow-up should look at:

- **Per-phase spans for the slow flows.** A send would cover tap → bridge
  call → wait for a bridge worker → Go work → RPC → render. Channel open and
  cold start would get the same breakdown.
- **Relaying them the same way.** Typed span names and attributes would go
  in the analytics batch, under the same consent, rate limit and privacy
  allowlist. The server would rebuild them as OTel spans with explicit
  timestamps, adjusted for clock offset, and export them through the
  existing OTLP pipeline.
- **Possibly trace context on selected RPCs** (send, daemon reply), so a
  client span becomes the parent of the server's span for the same request.
- **Sampling and cost**, for example uploading full traces only for flows
  over a latency threshold.
