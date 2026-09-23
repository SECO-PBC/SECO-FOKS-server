# Auto-deploy to Hetzner

Tag-triggered deploy from GitHub to the production FOKS server.

```
git tag v0.1.7-seco.1
git push origin v0.1.7-seco.1
       │
       ▼
GH Actions ─ build linux/arm64 (QEMU) ─ push ghcr.io/seco-pbc/foks-{server,tool}
       │
       ▼
SSH ─ /opt/foks/deploy.sh ─ pull → patch-db → compose up -d → health check
       │                                             (rollback on failure)
       ▼
   Hetzner CAX11 (ARM64)
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

KV-store shards **are** migrated automatically, like every other database: `patch-db --db kv-store` enumerates the configured shards itself (`PatchDBEng.loadShards`) and patches each one, each keeping its own `schema_patches` table. It prints a line per shard — check every shard after a kv-store patch, not just the first. `--shard <id>` remains available for patching one shard by hand.

This used to say the opposite, and that is how `foks_kv_store/p1` reached production unapplied in `v0.1.9-seco.21`: `kv-store` was missing from the list above, nobody ran the manual step, and because the server `SELECT`s the column p1 adds, every KV read failed behind a green deploy and a passing health check. `TestDeployScriptPatchesEveryPatchedDB` now fails the build if a database with registered patches is missing from the list, so **do not remove an entry to work around a failing patch.**

`patch-db` only patches databases that already exist — it cannot create one. A brand-new database (as `realtime` was for v0.1.7-seco.3) needs a one-off `foks-tool init-db --db <name>` first; the deploy script logs it as "not present on this server, skipping" until then. The base `.sql` schemas self-stamp their `schema_patches` rows, so a freshly init'd DB is already at the current patch level and the next `patch-db` reports "up to date".

## What lives where

| File                                     | Role                                  |
|------------------------------------------|---------------------------------------|
| `.github/workflows/deploy.yml`           | CI: build images, SCP script, run it  |
| `scripts/deploy/server-deploy.sh`        | Server: pull, migrate, restart, check |
| `server/foks-tool/patch_db.go` (`--yes`) | Non-interactive migration confirmation |
