/*
 * Read receipts (fork-only; dm-read-receipts in SECO-FOKS).
 *
 * user_channels.receipts: the row's own user says this channel is a 1:1 DM,
 * by passing RTReadThroughArg.dm on an explicit read. Only that user can set
 * it. A member's explicit read pointer reaches the other member
 * (RTInboxChannel.peerReadThrough) only on a channel with exactly two rows,
 * both marked, so the community daemon -- a member of every channel, which
 * never marks one -- can never make a channel qualify.
 *
 * user_inbox.read_receipts_off: the user's Read receipts switch, set by
 * rtSetReadReceipts. Off on either side withholds the pointer both ways.
 *
 * false for every existing row: receipts start with each member's first
 * marked read after the upgrade.
 */
ALTER TABLE user_channels ADD COLUMN receipts BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE user_inbox ADD COLUMN read_receipts_off BOOLEAN NOT NULL DEFAULT false;
