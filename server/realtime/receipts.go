package realtime

// Read receipts (fork-only; dm-read-receipts in SECO-FOKS).
//
// A member's explicit read pointer reaches the other member of a channel
// (RTInboxChannel.peerReadThrough) only when the channel has exactly two
// delivery rows, both rows' own users marked it as a 1:1 DM
// (user_channels.receipts, set by RTReadThroughArg.dm), and neither user has
// their Read receipts switch off (user_inbox.read_receipts_off). The server
// cannot tell a DM from a community -- team names are encrypted -- and a row
// count alone cannot either: the community daemon is a member of every
// channel, so a founder-and-daemon community also has two rows. Marking is
// per row and only by its own user, so a member can expose only their own
// pointer, and only to someone whose app says the same channel is a DM.
//
// Every writer here takes the user_inbox rows of BOTH members in one
// ascending-uid statement before touching user_channels: sends lock the inbox
// rows of a channel's members in ascending uid (fanoutInboxVersions), and two
// members reading their DM at once would otherwise each hold their own row
// while waiting for the other's.

import (
	"bytes"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
)

// peerReadThroughCol is the inbox query's peerReadThrough column, for the
// caller's row uc: the other row's explicit pointer when the channel
// qualifies, else 0. Interpolated by readChangedChannels after its
// per-channel state.
//
// The CASE short-circuits a row the caller never marked (every community
// channel), so the per-row count below only ever runs for DMs.
const peerReadThroughCol = `CASE WHEN NOT uc.receipts THEN 0 ELSE COALESCE((
		SELECT p.explicit_read_through FROM user_channels p
		JOIN user_inbox pi ON pi.short_host_id = p.short_host_id
		    AND pi.uid = p.uid AND pi.app_id = p.app_id
		WHERE p.short_host_id = uc.short_host_id
		AND p.channel_id = uc.channel_id
		AND p.uid <> uc.uid
		AND uc.receipts AND p.receipts
		AND NOT pi.read_receipts_off
		AND NOT EXISTS (SELECT 1 FROM user_inbox mi
		    WHERE mi.short_host_id = uc.short_host_id AND mi.uid = uc.uid
		    AND mi.app_id = uc.app_id AND mi.read_receipts_off)
		AND (SELECT count(*) FROM user_channels n
		    WHERE n.short_host_id = uc.short_host_id
		    AND n.channel_id = uc.channel_id) = 2
	), 0) END`

// dmPair is a two-row channel seen from one of its members.
type dmPair struct {
	peer         proto.UID
	callerMarked bool
	peerMarked   bool
}

// loadDmPair reads, without locks, the channel's delivery rows. It returns
// the pair when there are exactly two and the caller holds one, else nil. A
// membership change before the locks are taken costs at most one spurious
// bump, which a sync tolerates.
func loadDmPair(
	m shared.MetaContext,
	tx pgx.Tx,
	chid int64,
	caller proto.UID,
) (*dmPair, error) {
	rows, err := tx.Query(
		m.Ctx(),
		`SELECT uid, receipts FROM user_channels
		 WHERE short_host_id=$1 AND channel_id=$2
		 ORDER BY uid LIMIT 3`,
		m.ShortHostID(),
		chid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type member struct {
		uid    proto.UID
		marked bool
	}
	var members []member
	for rows.Next() {
		var raw []byte
		var mem member
		if err := rows.Scan(&raw, &mem.marked); err != nil {
			return nil, err
		}
		if err := mem.uid.ImportFromDB(raw); err != nil {
			return nil, err
		}
		members = append(members, mem)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(members) != 2 {
		return nil, nil
	}
	callerDB := caller.ExportToDB()
	for i, mem := range members {
		if bytes.Equal(mem.uid.ExportToDB(), callerDB) {
			other := members[1-i]
			return &dmPair{peer: other.uid, callerMarked: mem.marked, peerMarked: other.marked}, nil
		}
	}
	return nil, nil
}

// lockInboxes locks the given users' user_inbox rows for the app in
// ascending uid -- one statement, so the order holds within it -- and returns
// which of them have read receipts off. A user with no row reads as on.
func lockInboxes(
	m shared.MetaContext,
	tx pgx.Tx,
	appDB string,
	uids []proto.UID,
) (map[string]bool, error) {
	raw := make([][]byte, len(uids))
	for i, u := range uids {
		raw[i] = u.ExportToDB()
	}
	rows, err := tx.Query(
		m.Ctx(),
		`SELECT uid, read_receipts_off FROM user_inbox
		 WHERE short_host_id=$1 AND app_id=$2 AND uid = ANY($3)
		 ORDER BY uid
		 FOR UPDATE`,
		m.ShortHostID(),
		appDB,
		raw,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	off := make(map[string]bool, len(uids))
	for rows.Next() {
		var u []byte
		var o bool
		if err := rows.Scan(&u, &o); err != nil {
			return nil, err
		}
		off[string(u)] = o
	}
	return off, rows.Err()
}

// bumpInbox allocates the next inbox version for uid. The caller must already
// hold uid's user_inbox row lock, and must stamp exactly one delivery row at
// the version it returns (user_channels_inbox_idx).
func bumpInbox(
	m shared.MetaContext,
	tx pgx.Tx,
	uid proto.UID,
	appDB string,
) (int64, error) {
	var vers int64
	err := tx.QueryRow(
		m.Ctx(),
		`INSERT INTO user_inbox (short_host_id, uid, app_id, inbox_version, mtime)
		 VALUES ($1, $2, $3, 1, NOW())
		 ON CONFLICT (short_host_id, uid, app_id)
		 DO UPDATE SET inbox_version = user_inbox.inbox_version + 1, mtime = NOW()
		 RETURNING inbox_version`,
		m.ShortHostID(),
		uid.ExportToDB(),
		appDB,
	).Scan(&vers)
	return vers, err
}

// bumpAndStamp allocates the next inbox version for uid and stamps their
// delivery row in the channel at it -- one version per stamped row, the
// invariant user_channels_inbox_idx enforces. The caller must already hold
// uid's user_inbox row lock, and must not have updated that delivery row
// earlier in the transaction (see readThroughMarker.run on why).
//
// A row gone since the caller's unlocked read (a revoke) is skipped rather
// than bumped: a version with no row stamped at it would leave the head above
// anything a sync can fetch. The held inbox lock makes the check stable, as
// dropChannelMember takes it before deleting the row.
func bumpAndStamp(
	m shared.MetaContext,
	tx pgx.Tx,
	uid proto.UID,
	appDB string,
	chid int64,
) error {
	var one int
	err := tx.QueryRow(
		m.Ctx(),
		`SELECT 1 FROM user_channels
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
		m.ShortHostID(),
		chid,
		uid.ExportToDB(),
	).Scan(&one)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	vers, err := bumpInbox(m, tx, uid, appDB)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		m.Ctx(),
		`UPDATE user_channels
		 SET inbox_version=$4
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
		m.ShortHostID(),
		chid,
		uid.ExportToDB(),
		vers,
	)
	return err
}

// receiptsSetter flips the calling user's Read receipts switch and re-stamps
// both rows of every DM whose visible receipts that changes.
type receiptsSetter struct {
	arg rem.RtSetReadReceiptsArg
	uid proto.UID
	tx  pgx.Tx

	woken []proto.UID
}

type dmChannel struct {
	chid int64
	peer proto.UID
}

// markedDms lists, without locks, the caller's two-row channels where both
// rows are marked as a DM.
func (s *receiptsSetter) markedDms(m shared.MetaContext, appDB string) ([]dmChannel, error) {
	rows, err := s.tx.Query(
		m.Ctx(),
		`SELECT uc.channel_id, p.uid FROM user_channels uc
		 JOIN user_channels p ON p.short_host_id = uc.short_host_id
		     AND p.channel_id = uc.channel_id AND p.uid <> uc.uid
		 WHERE uc.short_host_id=$1 AND uc.uid=$2 AND uc.app_id=$3
		 AND uc.receipts AND p.receipts
		 AND (SELECT count(*) FROM user_channels n
		     WHERE n.short_host_id = uc.short_host_id
		     AND n.channel_id = uc.channel_id) = 2`,
		m.ShortHostID(),
		s.uid.ExportToDB(),
		appDB,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ret []dmChannel
	for rows.Next() {
		var d dmChannel
		var raw []byte
		if err := rows.Scan(&d.chid, &raw); err != nil {
			return nil, err
		}
		if err := d.peer.ImportFromDB(raw); err != nil {
			return nil, err
		}
		ret = append(ret, d)
	}
	return ret, rows.Err()
}

func (s *receiptsSetter) run(m shared.MetaContext) error {
	appDB, err := s.arg.AppID.ExportToDB()
	if err != nil {
		return err
	}
	dms, err := s.markedDms(m, appDB)
	if err != nil {
		return err
	}
	uids := []proto.UID{s.uid}
	seen := map[string]bool{string(s.uid.ExportToDB()): true}
	for _, d := range dms {
		if k := string(d.peer.ExportToDB()); !seen[k] {
			seen[k] = true
			uids = append(uids, d.peer)
		}
	}
	sortUIDs(uids)
	off, err := lockInboxes(m, s.tx, appDB, uids)
	if err != nil {
		return err
	}
	want := !s.arg.On
	if off[string(s.uid.ExportToDB())] == want {
		return nil
	}
	// The flag alone never moves inbox_version: a version bumped with no row
	// stamped at it would leave the head above anything a sync can fetch.
	_, err = s.tx.Exec(
		m.Ctx(),
		`INSERT INTO user_inbox (short_host_id, uid, app_id, inbox_version, mtime, read_receipts_off)
		 VALUES ($1, $2, $3, 0, NOW(), $4)
		 ON CONFLICT (short_host_id, uid, app_id)
		 DO UPDATE SET read_receipts_off = $4, mtime = NOW()`,
		m.ShortHostID(),
		s.uid.ExportToDB(),
		appDB,
		want,
	)
	if err != nil {
		return err
	}
	// Only DMs whose peer has receipts on change what either side sees.
	woken := map[string]bool{}
	for _, d := range dms {
		if off[string(d.peer.ExportToDB())] {
			continue
		}
		for _, u := range []proto.UID{s.uid, d.peer} {
			if err := bumpAndStamp(m, s.tx, u, appDB, d.chid); err != nil {
				return err
			}
			if k := string(u.ExportToDB()); !woken[k] {
				woken[k] = true
				s.woken = append(s.woken, u)
			}
		}
	}
	return nil
}

// SetReadReceipts sets the authenticated user's (m.UID()) Read receipts
// switch for the app.
func SetReadReceipts(m shared.MetaContext, arg rem.RtSetReadReceiptsArg) error {
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return err
	}
	defer rtdb.Release()
	return shared.RetryTx2(m,
		rtdb,
		"realtime.SetReadReceipts",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			s := receiptsSetter{arg: arg, uid: m.UID(), tx: tx}
			if err := s.run(m); err != nil {
				return nil, err
			}
			if len(s.woken) == 0 {
				return nil, nil
			}
			return func(m shared.MetaContext) {
				wakeInboxPollers(m, arg.AppID, s.woken)
			}, nil
		},
	)
}

// GetReadReceipts returns the authenticated user's Read receipts switch for
// the app: true when on, which a user with no inbox row yet is.
func GetReadReceipts(m shared.MetaContext, arg rem.RtGetReadReceiptsArg) (bool, error) {
	appDB, err := arg.AppID.ExportToDB()
	if err != nil {
		return false, err
	}
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return false, err
	}
	defer rtdb.Release()
	var off bool
	err = rtdb.QueryRow(
		m.Ctx(),
		`SELECT read_receipts_off FROM user_inbox
		 WHERE short_host_id=$1 AND uid=$2 AND app_id=$3`,
		m.ShortHostID(),
		m.UID().ExportToDB(),
		appDB,
	).Scan(&off)
	if err == pgx.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !off, nil
}
