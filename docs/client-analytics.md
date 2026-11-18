# Client analytics (fork-only)

Opt-in analytics from the SECO app, starting with end-to-end latency. The app
batches samples and reports them over the authenticated User protocol; the
user server turns them into `seco.client.*` metrics.

## Wire format

`proto-src/lib/analytics.snowp`:

- `ClientAnalyticsBatch{platform, latencies}`, where each latency is
  `{metric, ms}`.
- `ClientLatencyMetric` and `ClientPlatform` are **enums**, numbered from 1 so
  the zero value is never valid. A metric the server doesn't know can't be
  sent as a name; the only way to add one is a proto change.
- rem `User.reportClientAnalytics @201`. The SECO bridge calls it directly on
  the active user's User client; there is no agent-protocol (lcl) method.
- The batch limits live in `proto/lib/analytics_extras.go`
  (`MaxClientLatencySamples`, `MaxClientLatencyMs`), shared with the bridge,
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
  `client.metric` and `client.platform`.

The tag strings (`send_ack`, `ios`, ...) are spelled out in
`telemetry_client.go`, not taken from the proto's Go identifiers, so renaming
an enum member can't silently rename a Datadog series. Its helpers take the
proto enums and a typed drop reason, never strings, and
`TestClientTelemetryTags` fails if an enum member has no tag.

## Privacy

The RPC is authenticated, so the server knows who sent each batch, but none
of it is recorded: no uid, team, channel or device in any metric, span or
log. Tag values come only from enum names. Never add a dimension that could
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
