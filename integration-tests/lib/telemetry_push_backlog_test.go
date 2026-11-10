// The foks.rt.push_outbox.* gauges are fed by shared.PushOutboxBacklog,
// which runs from the metric export goroutine and only logs on failure. This
// runs its SQL against the real realtime schema so a broken query fails here
// rather than as a gauge that silently never appears in Datadog.
package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

func TestPushOutboxBacklogQuery(t *testing.T) {
	tew := testEnvBeta(t)
	m := tew.MetaContext()

	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()

	// A short_host_id no test host uses, so the push paths (which all filter
	// by host) never touch these rows. The probe counts across hosts.
	const host = 32001
	cleanup := func() {
		_, err := db.Exec(m.Ctx(), `DELETE FROM push_outbox WHERE short_host_id=$1`, host)
		require.NoError(t, err)
	}
	cleanup()
	defer cleanup()

	before, _, err := shared.PushOutboxBacklog(m.Ctx(), m.G())
	require.NoError(t, err)

	// One queued row from an hour ago and one sending row from now. Rows in
	// other states must not count.
	for _, r := range []struct {
		status string
		ago    string
	}{
		{"queued", "1 hour"},
		{"sending", "0 seconds"},
		{"done", "2 hours"},
		{"held", "2 hours"},
	} {
		_, err := db.Exec(m.Ctx(),
			`INSERT INTO push_outbox
			   (short_host_id, uid, channel_id, kind, status, ctime, mtime)
			 VALUES ($1, '\x00'::bytea, 1, 'msg', $2::push_status,
			         NOW() - $3::interval, NOW())`,
			host, r.status, r.ago)
		require.NoError(t, err)
	}

	after, oldest, err := shared.PushOutboxBacklog(m.Ctx(), m.G())
	require.NoError(t, err)
	require.Equal(t, before["queued"]+1, after["queued"])
	require.Equal(t, before["sending"]+1, after["sending"])
	require.Len(t, after, 2, "only queued and sending are reported")
	// The hour-old queued row is the oldest unless an earlier row is older;
	// the done/held rows (2h) must not count.
	require.GreaterOrEqual(t, oldest, 3600.0)
	require.Less(t, oldest, 7200.0)
}
