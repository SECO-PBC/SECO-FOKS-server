# Server observability (OpenTelemetry)

The FOKS server processes can export OpenTelemetry **metrics** and **traces** over OTLP/gRPC. In SECO staging they go to a Datadog Agent sidecar (setup: `scripts/deploy/README.md`, "Telemetry (Datadog)"). The code is vendor-neutral: anything that accepts OTLP works, including an OTel Collector, Grafana Alloy or Jaeger.

Telemetry is **off by default**. When it's off the server installs no-op providers, does not wrap RPC handlers, and attaches no pgx tracer, so the cost is a few nil-free calls.

**Not covered: the SECO daemon.** The daemon (`SECO-FOKS/apps/daemon`) is not instrumented. Its RPCs to this server are measured server-side like any client's, and can't be told apart from phone traffic. Nothing inside it is measured: poll lag, AI backend latency, its own queueing. Daemon telemetry is deferred. It has its own constraints: it runs in a TEE with plaintext access, and dstack can't run an agent sidecar.

## Privacy rule

SECO promises end-to-end encryption, and the team has ruled out client-style telemetry (2026-06-19). Server telemetry therefore carries **only aggregate, non-identifying attributes**:

| Allowed | Never allowed |
|---|---|
| server type (`service.name`), RPC method name, proto status code name, DB type, SQL operation keyword, static transaction names (`shared.TxName`), phase/op/outcome/result enums, counts | UIDs, usernames, FQNs, team / channel / device IDs, user host names, remote IPs, message metadata, SQL parameters, raw error strings |

How the code enforces this:
- **RPC spans** record the proto status code name, never `err.Error()`.
- **pgx spans** record the SQLSTATE, never the Postgres message (messages can echo values). They are named `pg.<OP>`. SQL text is recorded only when `db_statement: true`; all server SQL is parameterized (`$n`), and parameters are never recorded.
- **Container logs are not shipped.** zap logs can contain UIDs and remote IPs. Datadog log collection is off in the sidecar config.
- **The `allowedTelemetryKeys` test** in `server/shared/telemetry_test.go` is the allowlist. Every exported attribute key must be in it, and a planted user ID must not appear in any attribute, span name, status or event. Adding a key is a privacy decision.

## Configuration

This is the `telemetry` block in the server jsonnet config. It is present but disabled in `conf/srv/*.jsonnet`; turn it on per host in `local.pre.libsonnet`'s `final()`.

| Key | Default | |
|---|---|---|
| `enabled` | `false` | |
| `otlp_endpoint` | `datadog-agent:4317` | OTLP/gRPC `host:port` |
| `secure` | `false` | TLS to the endpoint; the sidecar is on the private compose network |
| `env` | — | becomes `deployment.environment.name` (Datadog `env`). Must match the agent's `DD_ENV`; a warning is logged if empty |
| `trace_sample_ratio` | `1.0` | fraction of root spans kept |
| `trace_sample_methods` | `{}` | per-RPC overrides of `trace_sample_ratio`, keyed `"<Protocol>.<method>"` |
| `metric_interval_ms` | `10000` | export period |
| `db_statement` | `false` | record parameterized SQL text on DB spans |

The resource sets `service.name = foks-<server type>`, e.g. `foks-realtime` or `foks-user`. Counters and histograms use **delta** temporality. Histograms are **base-2 exponential**, which Datadog turns into distributions, so p50, p95 and p99 are accurate without picking bucket boundaries.

## Metrics

| Metric | Type | Attributes | Where |
|---|---|---|---|
| `foks.rpc.server.duration` | histogram, ms | `rpc.method`, `outcome` | every RPC (`shared.InstrumentProtocol`, applied by the `ProtocolRegistrar` that `serveConnection` passes to `RegisterProtocols`) |
| `foks.rpc.server.errors` | counter | `rpc.method`, `error.code` | |
| `foks.rpc.server.active` | up-down | `rpc.method` | in-flight handlers |
| `foks.rpc.server.connections` | up-down | — | open client connections |
| `foks.db.query.duration` | histogram, ms | `db.type`, `db.operation.name`, `outcome` | every pgx query (`pgTracer`) |
| `foks.db.query.errors` | counter | `db.type`, `db.operation.name`, `db.response.status_code` | |
| `foks.db.pool.acquire.duration` | histogram, ms | `db.type` | wait to acquire a pooled connection |
| `foks.db.pool.{acquired,idle,total,max}` | gauge | `db.type` (+ `db.kv_shard`) | `pgxpool.Stat()` |
| `foks.db.pool.empty_acquires`, `foks.db.pool.acquire_wait` | counter | `db.type` | acquires that had to wait, and for how long |
| `foks.db.tx.duration` | histogram, ms | `db.tx.name`, `outcome` | `RetryTx`/`RetryTx2`, from the first BEGIN to the final COMMIT, including retries but **not** the success hook |
| `foks.db.tx.post_commit.duration` | histogram, ms | `db.tx.name` | the `RetryTx2` success hook, run after COMMIT (for sends, the wake) |
| `foks.db.tx.retries` | counter | `db.tx.name` | retries after a COMMIT that never reached the server (connection-level; rare). Deadlocks and serialization failures are **not** retried |
| `foks.rt.phase.duration` | histogram, ms | `phase` (+ `op` on `wake`) | realtime: `lock`, `prune`, `insert`, `fanout`, `wake`, `reconcile` |
| `foks.rt.fanout.members` | histogram | `op` | members woken per wake. `op`: `send`, `read_mark`, `receipts`, `mute`, `acl`, `channel`, `reconcile` |
| `foks.rt.poll.duration` | histogram, ms | `outcome` | `rtPollInbox`: `bumped_immediate`, `bumped`, `timeout`, `cancelled`, `error` |
| `foks.rt.reconcile` | counter | `result` | late-join fan-in: `memory`, `cursor` or `full` (slow path) |
| `foks.rt.pollers.parked` | gauge | — | parked long-polls (realtime only) |
| `foks.rt.push_outbox.backlog` | gauge | `status` | `queued`/`sending` rows, sampled every 30s (realtime only) |
| `foks.rt.push_outbox.oldest_age` | gauge, s | — | age of the oldest `queued`/`sending` row; 0 when there are none (use `backlog` to tell an empty queue from a just-queued row). Alert on it exceeding a few minutes. A gap in either gauge means the probe skipped that sample (realtime pool saturated) or failed (logged at Warn once) |
| Go runtime (`go.*`) | various | — | `contrib/instrumentation/runtime` |

`rtPollInbox` is a long poll (up to 55s), so its `foks.rpc.server.duration` measures wait time, not work. Filter it out of RPC latency views, and use `foks.rt.poll.duration` by `outcome` instead.

## Trace sampling

Each host's sampling lives in its own config, so it is set per environment. Staging traces everything (`1.0`, no overrides), which is fine at its current traffic.

Sampling is decided once per trace, at the root RPC span, and every child span follows that decision. `trace_sample_methods` overrides the default for individual RPCs. Unsampled traces cost almost nothing: their child and query spans are never created. Metrics are recorded for every call either way.

**At high traffic, sample down.** With ~1000 parked long-polls and 10 sends/s, each send wakes ~1000 `rtPollInbox` calls, and each woken client then syncs. That's ~20k traces/s and roughly 80k spans/s, which fills the span queue (dropped spans are not logged), costs real server CPU, and is expensive to ingest. For prod or a heavy benchmark, something like:

```jsonnet
telemetry +: {
    enabled : true, env : "prod",
    trace_sample_ratio : 0.01,
    trace_sample_methods : { "RealTime.rtSend" : 1.0, "RealTime.rtPollInbox" : 0 },
}
```

Override names are checked only for shape (`Protocol.method`) and range. A misspelled method name matches nothing and silently falls back to the default, so confirm in the APM service list that the override took effect. When sampled, Datadog's APM hit counts and latency pages undercount; use the `foks.rpc.server.*` metrics for rates and percentiles.

## Traces

- **One root span per RPC**, named `<Protocol>.<method>`. The snowpack wire format carries no trace context, so spans are not linked across services (for example `user` → `merkle_query`). Each service's traces stand alone.
- **Child spans:** `rt.<phase>`, `pg.<OP>` per query, and for each `RetryTx`:
  ```
  db.tx  <name>                      first BEGIN → final COMMIT
    db.tx.attempt  #0  result=retry   each try, with its queries
    db.tx.attempt  #1  result=committed
  db.tx.post_commit  <name>          success hook, a sibling of db.tx
  ```
  An attempt's `result` is `committed`, `retry`, `error`, or `exhausted` (a retryable failure on the last of 5 tries). Attempt spans cover only BEGIN through COMMIT; the backoff between attempts counts toward `db.tx` but not toward any attempt. `foks.db.tx.retries` counts retries actually performed (at most 4).
  `TxName` values become the `db.tx.name` label, so they must be fixed strings.
- **Query spans only exist under a sampled parent.** Background loops record query metrics but produce no orphan spans.

## Diagnosing the realtime send path

For one `RealTime.rtSend`, read the following together:

1. **`foks.rt.phase.duration` by `phase`.** `lock` is time waiting on the channel row's `FOR UPDATE`, so all sends to one channel run one at a time. `fanout` is the set-based `user_inbox`/`user_channels`/`push_outbox` writes. `wake` is post-commit hub wakes, which take one global lock per member.
   **Filter `wake` on `op:send`.** Every read mark, mute, ACL change and late-join reconcile also wakes pollers (usually 1–2 members). They far outnumber sends, so an unfiltered `wake` p95 describes them, not sends.
2. **`foks.rt.fanout.members{op:send}`** to see how fan-out cost scales with channel size. The other `op` values show wake load from non-send traffic, which contends for the same hub lock.
3. **`foks.db.pool.acquire.duration` and `foks.db.pool.empty_acquires`** for pool starvation.
4. **Deadlocks and failed sends:** `foks.db.query.errors{db.response.status_code:40P01}` and `foks.db.tx.duration{db.tx.name:realtime.SendMessage, outcome:error}`. `RetryTx2` deliberately doesn't retry deadlocks or other server errors; it retries only a COMMIT that never reached the server, so `foks.db.tx.retries` should stay near zero. Contention between sends shows up as `lock` time instead.
5. **`foks.db.tx.post_commit.duration{db.tx.name:realtime.SendMessage}`** for the wake cost outside the transaction.
6. **Datadog DBM → Query Metrics** for the `INSERT INTO user_inbox ... SELECT` fan-out statement: lock waits, rows and I/O.
