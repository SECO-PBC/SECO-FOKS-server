-- p2: the team system store (fork-only; design in docs/kv-system-store.md).
--
-- daemon_locked marks a directory only a Sage account may change. FALSE is
-- right for every existing row, so there is no backfill. system_root holds at
-- most one row per team: the store's root, reached by no dirent, and insert-
-- only so it can never be replaced.
--
-- Operator note: foks_kv_store is sharded, and patch-db applies patches per
-- shard, each shard keeping its own schema_patches table. Confirm every
-- shard reports id 2 before calling the rollout done.

ALTER TABLE dir ADD COLUMN daemon_locked BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE system_root (
    short_host_id SMALLINT NOT NULL,
    party_id BYTEA NOT NULL,
    dir_id BYTEA NOT NULL,
    version INT NOT NULL,
    ptk_gen INT NOT NULL,
    read_role_type SMALLINT NOT NULL,
    read_role_viz_level SMALLINT NOT NULL,
    binding_mac BYTEA NOT NULL,
    ctime TIMESTAMP NOT NULL,
    PRIMARY KEY(short_host_id, party_id)
);
