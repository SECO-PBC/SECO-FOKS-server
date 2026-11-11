-- p3: find a dirent's active version without scanning its history.
--
-- Every put deactivates the previous version of the entry
-- (direntUpdater, server/kv-store/dirent.go). That UPDATE filters on
-- `active`, and this index, which holds only active rows (one per live
-- entry), lets Postgres find that single row directly. Without it, the
-- lookup walks every older version of the entry through the primary key,
-- and staging had entries with tens of thousands of versions.
--
-- The index is small (one row per live entry) and builds quickly. It is a
-- plain CREATE INDEX, which blocks writes to dirent while it builds.
--
-- Operator note: foks_kv_store is sharded, and patch-db applies patches per
-- shard, each shard keeping its own schema_patches table. Confirm every
-- shard reports id 3 before calling the rollout done.

CREATE INDEX dirent_active_key_idx
    ON dirent(short_host_id, short_party_id, dir_id, dirent_id)
    WHERE (active = true);
