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
- rem `User.reportClientAnalytics @201`; the agent forwards lcl
  `Admin.reportClientAnalytics @200` to it.

## What the server does (`shared.RecordClientAnalytics`)

- Refuses the whole batch for an unknown platform, more than 512 samples, or
  over the per-user rate (one batch per 5s, burst 10, in memory).
- Drops and counts individual samples that name an unknown metric (a newer
  client talking to an older server) or exceed 10 minutes, so version skew
  shows in `seco.client.latency.dropped` instead of failing clients.
- Records the rest in `seco.client.latency` (milliseconds), tagged only with
  `client.metric` and `client.platform`.

## Privacy

The RPC is authenticated, so the server knows who sent each batch, but none
of it is recorded: no uid, team, channel or device in any metric, span or
log. Tag values come only from enum names. Never add a dimension that could
identify a user, team, channel or device, and never put free text in a batch.

Opting in is a client setting; the server can't tell an opted-in client from
any other and doesn't try.

## Adding a metric

1. Add it to `ClientLatencyMetric` (or a new enum, for a new kind of
   measurement) and regenerate.
2. Tag a fork release and bump the pin in SECO-FOKS, then add the name to
   the app's TS union (a parity test there compares the two).
3. Deploy the server before shipping a client that sends it; until then the
   server drops it as `unknown_metric`.

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
