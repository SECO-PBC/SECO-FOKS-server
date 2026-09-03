
/*
 * Private channels (fork-only; see docs/rt-private-channel-acl.md).
 *
 * Privacy is an orthogonal boolean axis, NOT a third channel_tier enum value.
 * The patch runner applies each patch inside one transaction
 * (server/shared/patch.go), and Postgres forbids USING an enum value added by
 * ALTER TYPE ... ADD VALUE in the same transaction that added it -- so an
 * enum-extending patch breaks the moment any statement here, or in a
 * same-run later patch, references the new value. The boolean also keeps the
 * PG enum identical to upstream's, which matters because this fork keeps
 * merging from upstream. Do not "simplify" this back into channel_tier.
 *
 * tier still says which role floor may learn a channel exists and which key
 * seals its name; private additionally demands an explicit channel_acl row.
 */
ALTER TABLE channels ADD COLUMN private BOOLEAN NOT NULL DEFAULT false;

/*
 * channel_acl: the authoritative membership table for private channels.
 *
 * Deliberately NOT user_channels: that is denormalized delivery state which
 * lingers after a team leave, is self-healed by the late-join fan-in, and
 * doubles as per-user inbox prefs. Conflating "may read" with "is being
 * delivered to" is exactly the ambiguity that produces a silent, retroactive
 * privacy failure. The invariant the two share -- for a private channel the
 * set of user_channels rows equals the set of channel_acl rows -- is
 * maintained by grant/revoke/create and asserted by tests, not by a schema
 * constraint.
 */
CREATE TABLE channel_acl (
    short_host_id SMALLINT NOT NULL,
    channel_id BIGINT NOT NULL,
    uid BYTEA NOT NULL,
    acl_role SMALLINT NOT NULL, /* 0 = member, 1 = owner (may grant/revoke) */
    granted_by BYTEA NOT NULL, /* uid of the granter; audit trail, shown to members */
    ctime TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(short_host_id, channel_id, uid),
    FOREIGN KEY(short_host_id, channel_id) REFERENCES channels(short_host_id, channel_id)
);
/* "which private channels is this user in", for the set-based listing gate */
CREATE INDEX channel_acl_uid_idx ON channel_acl(short_host_id, uid);
