package realtime

// Read pushes (fork-only; cross-device-read-sync in SECO-FOKS). When a user's
// explicit read pointer in a channel moves -- they read it on one device, or
// sent into it -- the pushes about what they just read are settled in the
// same transaction:
//
//   - Rows that have not gone out (queued, or held for the daemon) are deleted:
//     the user already saw those messages, so no device should buzz for them.
//   - If a row in the newly read range already went out (sending or done), one
//     'read' row is queued for the user. The relay sends it as a silent
//     background push carrying the channel's id, and the user's devices remove
//     that channel's notifications. The (old, new] window makes that one read
//     push per delivered batch.
//
// Only the explicit pointer settles pushes: read_through also moves on every
// page a client fetches (including older builds polling in the background),
// which says nothing about what the user saw.

import (
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
)

// settleReadPushes runs in the transaction that moved uid's explicit pointer
// in channelID from oldSeq to newSeq (newSeq > oldSeq).
func settleReadPushes(
	m shared.MetaContext,
	tx pgx.Tx,
	uid proto.UID,
	channelID int64,
	oldSeq int64,
	newSeq int64,
) error {
	_, err := tx.Exec(
		m.Ctx(),
		`DELETE FROM push_outbox
		 WHERE short_host_id=$1 AND uid=$2 AND channel_id=$3
		   AND kind='msg' AND status IN ('queued', 'held') AND seq <= $4`,
		m.ShortHostID(),
		uid.ExportToDB(),
		channelID,
		newSeq,
	)
	if err != nil {
		return err
	}
	// The row carries the full channel id: it is what the devices tag a
	// channel's notifications with. Nothing to take back without a token to
	// send it to.
	_, err = tx.Exec(
		m.Ctx(),
		`INSERT INTO push_outbox (short_host_id, uid, channel_id, kind, seq, data, status, ctime, mtime)
		 SELECT $1, $2, $3, 'read', $5, c.channel_id_full, 'queued', NOW(), NOW()
		   FROM channels c
		  WHERE c.short_host_id=$1 AND c.channel_id=$3
		    AND EXISTS (
		        SELECT 1 FROM push_outbox p
		         WHERE p.short_host_id=$1 AND p.uid=$2 AND p.channel_id=$3
		           AND p.kind='msg' AND p.status IN ('sending', 'done')
		           AND p.seq > $4 AND p.seq <= $5)
		    AND EXISTS (
		        SELECT 1 FROM push_tokens t
		         WHERE t.short_host_id=$1 AND t.uid=$2 AND t.enabled)`,
		m.ShortHostID(),
		uid.ExportToDB(),
		channelID,
		oldSeq,
		newSeq,
	)
	return err
}
