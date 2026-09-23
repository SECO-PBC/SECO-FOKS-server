
/*
 * Channel archive (fork-only; see docs/rt-channel-mutation.md).
 *
 * An archived channel is closed to new activity and leaves the inbox, but it
 * is not deleted: every message, party, ACL and delivery row stays. NULL means
 * live, so existing channels need no backfill. Only the paths that
 * deliberately exclude archived channels test it -- today just the late-join
 * fan-in's anti-join. The team listing, the inbox delta and reads by explicit
 * channel id all still serve an archived channel, each for its own reason
 * (see below and docs/rt-channel-mutation.md).
 *
 * A nullable timestamp rather than a boolean, because "when was this closed"
 * is the question that gets asked afterwards and the column costs the same.
 *
 * Note the archived channel deliberately STAYS in rtListAllChannelsForTeam.
 * The channel set doubles as the team's name registry -- names are
 * PTK-encrypted, so only clients can compare them, and a client can only
 * refuse a duplicate name it can see. Dropping archived rows from the listing
 * would free the name and make every unarchive a potential collision that
 * nothing on the server could detect. What archive removes is the channel's
 * presence in the INBOX (rtGetChangedThreads) and the late-join fan-in.
 *
 * Fork patch numbering diverges from upstream's for this database: our p5 is
 * private channels while upstream's p5 is no-push, and patch identity is a
 * bare integer with no content hash. Reconcile by CONTENT at the next upstream
 * merge, not by id -- see SECO-UPSTREAM.md.
 */
ALTER TABLE channels ADD COLUMN archived_at TIMESTAMPTZ;
