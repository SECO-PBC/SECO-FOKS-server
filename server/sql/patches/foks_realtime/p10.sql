/*
 * Explicit read pointer (fork-only; cross-device-read-sync in SECO-FOKS).
 *
 * read_through moves on every page a client fetches (librt's GetThreadView
 * reads as "viewing implies reading"), so it says "some device downloaded
 * this", not "the user saw it". explicit_read_through moves only on an
 * rtReadThrough that says so (RTReadThroughArg.explicit) and on the user's
 * own send. Clients sync read state across devices from it, and moving it
 * settles the user's pushes in that channel (realtime.settleReadPushes).
 *
 * 0 for every existing row: reads made before this patch are unknown, so an
 * upgrade marks nothing read.
 */
ALTER TABLE user_channels ADD COLUMN explicit_read_through BIGINT NOT NULL DEFAULT 0;

/*
 * settleReadPushes looks up one user's message pushes in one channel by seq,
 * on every explicit read and every send. push_outbox_user_idx is ordered by
 * ctime, so without this it would scan all of the user's rows -- a week's
 * worth once the relay keeps done rows that long -- and the community daemon
 * is a member of every channel.
 */
CREATE INDEX push_outbox_user_channel_idx ON push_outbox(short_host_id, uid, channel_id, seq);
