/*
 * Delegated push release, part 2 (fork-only).
 *
 * push_holds: at most one per (team, app). While the holder is an active team
 * member, a send into any channel of the team that the holder can read writes
 * its push_outbox rows as 'held' (p7). Placing and clearing a hold needs team
 * admin; release and notify need the holder. Carries nothing but who holds.
 */
CREATE TABLE push_holds (
    short_host_id SMALLINT NOT NULL,
    team_id BYTEA NOT NULL,
    app_id app_id NOT NULL,
    holder_uid BYTEA NOT NULL,
    ctime TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(short_host_id, team_id, app_id)
);

/* The holder's release and hold-end scans: a channel's (or team's) held rows. */
CREATE INDEX push_outbox_held_idx ON push_outbox(short_host_id, channel_id, uid, seq) WHERE status = 'held';
