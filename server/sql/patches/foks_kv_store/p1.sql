-- p1: channel-scoped storage for the realtime private-channel ACL
-- (fork-only; design in docs/kv-channel-acl.md).
--
-- Adds a nullable channel tag to every node type, and a registry of each
-- channel's storage root. NULL means "not channel storage": every existing
-- row is correct with no backfill, and untagged behaviour is unchanged. The
-- tag is the short channel id (RTChannelIDShort, a BIGINT), the same value
-- channel_acl.channel_id stores in foks_realtime; the kv-store server checks
-- membership against that table across databases.
--
-- Operator note: foks_kv_store is sharded, and patch-db applies patches per
-- shard, each shard keeping its own schema_patches table. Confirm every
-- shard reports id 1 before calling the rollout done.

ALTER TABLE dir ADD COLUMN channel_id BIGINT;
ALTER TABLE large_file ADD COLUMN channel_id BIGINT;
ALTER TABLE small_file_or_symlink ADD COLUMN channel_id BIGINT;

CREATE INDEX dir_channel_idx ON dir(short_host_id, short_party_id, channel_id)
    WHERE (channel_id IS NOT NULL);

/*
 * One registered storage root per channel: the only place a tagged directory
 * may hang beneath an untagged parent (containment is enforced on every
 * kvPut link). Rows are written by kvChannelMkRoot, an ACL-owner/team-admin
 * act. No foreign key to dir: its primary key includes version, and the row
 * must survive the root directory's rotations.
 */
CREATE TABLE channel_kv_root (
    short_host_id SMALLINT NOT NULL,
    short_party_id BYTEA NOT NULL,
    channel_id BIGINT NOT NULL,
    dir_id BYTEA NOT NULL,
    ctime TIMESTAMP NOT NULL,
    PRIMARY KEY(short_host_id, short_party_id, channel_id)
);
