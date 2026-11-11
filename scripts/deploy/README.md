# Auto-deploy to Hetzner

Tag-triggered deploy from GitHub to the production FOKS server.

```
git tag v0.1.7-seco.1
git push origin v0.1.7-seco.1
       │
       ▼
GH Actions ─ build linux/amd64 ─ push ghcr.io/seco-pbc/foks-{server,tool}
       │
       ▼
SSH ─ /opt/foks/deploy.sh ─ pull → patch-db → compose up -d → health check
       │                                             (rollback on failure)
       ▼
   Hetzner server (x86_64)
```

## One-time setup

### 1. GitHub repository secrets

Settings → Secrets and variables → Actions → New repository secret:

| Name             | Value                                                             |
|------------------|-------------------------------------------------------------------|
| `DEPLOY_HOST`    | Server IP or hostname (e.g. `foks.example.com`)                   |
| `DEPLOY_USER`    | `root`                                                            |
| `DEPLOY_SSH_KEY` | Private key (full PEM, including header/footer) for the deploy user |

`GITHUB_TOKEN` is provided automatically and handles GHCR push + pull.

### 2. SSH key on the server

On a workstation:

```bash
ssh-keygen -t ed25519 -f foks-deploy -N "" -C "github-actions-foks-deploy"
ssh-copy-id -i foks-deploy.pub root@<server>
# paste the contents of `foks-deploy` into the DEPLOY_SSH_KEY GitHub secret
# delete the local copy of the private key
```

### 3. Server prerequisites

The server must already be running FOKS (per `foks-server-setup.md`) with:

- `/opt/foks/workdir/docker-compose.yml`
- `/opt/foks/workdir/.env`
- `/opt/foks/workdir/conf-guest/`, `/opt/foks/workdir/keys/`
- `docker compose ps` showing the stack up

The first deploy will:

1. Back up the existing compose file to `docker-compose.yml.pre-ci.bak`.
2. Replace the hardcoded `image: ghcr.io/foks-proj/foks-server:...` lines with `image: ${FOKS_SERVER_IMAGE}`.
3. Append `FOKS_SERVER_IMAGE=...` to `.env` (initially seeded with the old image — that is the rollback target if the very first deploy fails).

This is idempotent: subsequent deploys skip the rewrite.

The rewrite only touches `docker-compose.yml`. A service defined in `docker-compose.override.yml` keeps whatever image it names, unless that file also says `image: ${FOKS_SERVER_IMAGE}`. Servers bootstrapped by `scripts/srv/build.bash` after `realtime` was added to its service list get `realtime` in `docker-compose.yml` and need no override.

## Triggering a deploy

**On tag push:**

```bash
git tag v0.1.7-seco.1
git push origin v0.1.7-seco.1
```

**Manual re-run** (e.g. retry without re-tagging): GitHub → Actions → "Build & Deploy" → Run workflow → enter tag.

## Rollback

Automatic on health-check failure (TCP probe on `127.0.0.1:4430`, 90 s timeout).

Manual rollback to a previous tag — re-run the workflow with the old tag, or on the server:

```bash
ssh root@<server>
sed -i 's|^FOKS_SERVER_IMAGE=.*|FOKS_SERVER_IMAGE=ghcr.io/seco-pbc/foks-server:v0.1.7-seco.0|' \
    /opt/foks/workdir/.env
cd /opt/foks/workdir && docker compose up -d
```

## DB migrations

`foks-tool patch-db --yes --db <name>` runs once per database on every deploy. It is idempotent (already-applied patches are skipped via the `schema_patches` table). The deploy script iterates over: `server-config users beacon merkle-tree merkle-raft merkle-raft-archive queue-service realtime kv-store`.

KV-store shards **are** migrated automatically, like every other database: `patch-db --db kv-store` enumerates the configured shards itself (`PatchDBEng.loadShards`) and patches each one, each keeping its own `schema_patches` table. `--shard <id>` remains available for patching one shard by hand.

**Do not try to confirm the rollout from the deploy output.** `run_patch` captures patch-db's output and logs only `kv-store: patched`, and patch-db itself prints nothing for a shard that had no pending patches (`PatchSummary.addPatches` skips it) — exiting with "no patches to apply" when every shard is already current. A silent shard and an up-to-date shard look identical. Ask the database instead:

```bash
for d in $(docker compose exec -T postgresql psql -U foks -d postgres -lqt | cut -d'|' -f1 | tr -d ' ' | grep '^foks_kv_store_'); do
  echo -n "$d: "; docker compose exec -T postgresql psql -U foks -d "$d" -tAc 'SELECT id FROM schema_patches ORDER BY 1;'
done
```

Every shard must list every patch id.

This used to say the opposite, and that is how `foks_kv_store/p1` reached production unapplied in `v0.1.9-seco.21`: `kv-store` was missing from the list above, nobody ran the manual step, and because the server `SELECT`s the column p1 adds, every KV read failed behind a green deploy and a passing health check. `TestDeployScriptPatchesEveryPatchedDB` now fails the build if a database with registered patches is missing from the list, so **do not remove an entry to work around a failing patch.**

`patch-db` only patches databases that already exist — it cannot create one. A brand-new database (as `realtime` was for v0.1.7-seco.3) needs a one-off `foks-tool init-db --db <name>` first; the deploy script logs it as "not present on this server, skipping" until then. The base `.sql` schemas self-stamp their `schema_patches` rows, so a freshly init'd DB is already at the current patch level and the next `patch-db` reports "up to date".

## Telemetry (Datadog)

The server can export OpenTelemetry metrics and traces over OTLP to a Datadog Agent sidecar. The agent also collects host and container metrics, plus Postgres Database Monitoring. Telemetry is off unless `telemetry.enabled` is set. See `docs/observability.md` for the metric list and the privacy rule.

One-time setup on the server, in `/opt/foks/workdir`:

1. **Secrets.** Add these to `.env`:
   - `DD_API_KEY=<key>`
   - `DD_SITE=<site>` (for example `datadoghq.com` or `datadoghq.eu`)
   - `DD_PG_PASSWORD=<random>`
   - `DD_ENV=staging` (optional; this is the default). It must match `telemetry.env` in step 5: the agent tags host, container and Postgres metrics with `DD_ENV`, and the servers tag `foks.*` metrics and traces with `telemetry.env`. If they differ, Datadog splits the two under different `env` values.
2. **Load the sidecar.** Copy `scripts/deploy/datadog/docker-compose.datadog.yml` into the workdir. Then add this line to `.env`:

   `COMPOSE_FILE=docker-compose.yml:docker-compose.override.yml:docker-compose.datadog.yml`

   Setting `COMPOSE_FILE` turns off automatic loading of the override file, so it has to be listed. Do **not** copy the file over `docker-compose.override.yml`, which already holds the port mappings and the Postgres `max_connections` flag.
3. **Turn on `pg_stat_statements`.** In `docker-compose.override.yml`, extend the existing `postgresql` command as shown below. Keep it in that file: a second file that also sets `command` would replace the whole line and drop `max_connections`. Then run `docker compose up -d postgresql`; this briefly restarts Postgres.
   ```yaml
   command: ["postgres", "-c", "max_connections=200",
             "-c", "shared_preload_libraries=pg_stat_statements",
             "-c", "pg_stat_statements.track=all", "-c", "track_io_timing=on"]
   ```
4. **Create the monitoring role.** From `scripts/deploy/datadog/`, run `(set -a; . /opt/foks/workdir/.env; set +a; ./pg-setup.sh)`. This loads `DD_PG_PASSWORD` from `.env`, so the password never appears in your shell history or on a command line. The script is idempotent. It restricts the `SECURITY DEFINER` explain helper to the `datadog` role, so if you ran an earlier version of the script, run this one again.
5. **Point the servers at the agent.** Add `telemetry +: { enabled : true, env : "staging" },` to `final()` in `conf-guest/local.pre.libsonnet`. `env` must equal `DD_ENV` from step 1; servers log a warning at startup if it is empty.
6. **Start the agent and restart the servers.** Run `docker compose up -d` to start `datadog-agent`; this also recreates `postgresql` with its new labels. Then restart the FOKS servers so they re-read their config: `docker compose restart $(docker compose config --services | grep -vxE 'postgresql|datadog-agent')`. A plain `up -d` doesn't do this, because editing `local.pre.libsonnet` changes no compose definition. If you finish these steps *before* deploying a tag, the deploy recreates the servers anyway. Later CI deploys run `docker compose up -d`, which picks up `COMPOSE_FILE` and keeps the agent running.

**Check that it works:**
- `docker compose exec datadog-agent agent status` shows the OTLP receiver and the `postgres` check as OK.
- Each server logs `telemetry stage=enabled` at startup.
- `foks.*` metrics appear in Datadog within a minute.

**Deploy markers:** when `.env` has `DD_API_KEY`, `server-deploy.sh` posts a Datadog change event when a deploy starts and another when it ends. The end event is `deploy_phase:end` on success, or `deploy_phase:failed` on any failure, including a health-check rollback. Events go to the v2 events API on `event-management-intake.<DD_SITE>`, because this org has the v1 events API turned off. Datadog also files each event under `source:change_tracking`. Each event is tagged `deploy`, `env:<DD_ENV>`, `service:foks-server` and `version:<tag>`, and the pair shares an `aggregation_key`. The timeseries widgets in `docs/ops/datadog-*.json` overlay `tags:deploy $env`. To overlay only the moment new code went live, use `tags:deploy deploy_phase:end $env`. If the event can't be sent, the deploy logs a warning and carries on.

**Turn it off:** set `enabled : false` (or remove the line), then restart the servers with the same `docker compose restart ...` command. To also remove the agent, take `docker-compose.datadog.yml` out of `COMPOSE_FILE`.

**Security:** the agent mounts the Docker socket, which gives it root-equivalent access to the host. `:ro` doesn't limit the API. We accept this for staging, for container metrics and label-based Postgres autodiscovery. The image is pinned to an exact version (`agent:7.85.3`), so upgrades happen only when that line changes. Revisit before running the agent in prod; `docker-compose.datadog.yml` lists the alternatives.

**Capacity:** the staging host has 2 vCPU and 2 GB RAM. The agent is capped at 512 MB and 0.5 CPU, but it still shares the box with the server, so compare benchmark runs with telemetry on and off.

## What lives where

| File                                     | Role                                  |
|------------------------------------------|---------------------------------------|
| `.github/workflows/deploy.yml`           | CI: build images, SCP script, run it  |
| `scripts/deploy/server-deploy.sh`        | Server: pull, migrate, restart, check |
| `server/foks-tool/patch_db.go` (`--yes`) | Non-interactive migration confirmation |
| `scripts/deploy/datadog/`                | Datadog Agent sidecar + Postgres DBM setup (telemetry) |
