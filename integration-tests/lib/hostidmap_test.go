// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"context"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/integration-tests/common"
	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

func TestHostIdMapFilter(t *testing.T) {
	env := globalTestEnv.Fork(t, common.SetupOpts{})
	defer func() {
		_ = env.ShutdownFn()
	}()
	m := env.MetaContext()

	mainId := m.G().ShortHostID()
	start := time.Now().Add(time.Hour * 100)

	db, err := m.Db(shared.DbTypeServerConfig)
	require.NoError(t, err)
	defer db.Release()

	ins := func(parent core.ShortHostID, vhost core.ShortHostID, tm time.Time) {
		vhid, err := proto.RandomID16er[proto.VHostID]()
		require.NoError(t, err)
		var hid proto.HostID
		err = core.RandomFill(hid[:])
		require.NoError(t, err)
		tag, err := db.Exec(m.Ctx(),
			`INSERT INTO hosts(short_host_id, host_id, vhost_id, 
			   root_short_host_id, 
			   parent_short_host_id, ctime)
			 VALUES($1, $2, $3, $4, $5, $6)`,
			vhost.ExportToDB(),
			hid.ExportToDB(),
			vhid.ExportToDB(),
			parent.ExportToDB(),
			parent.ExportToDB(),
			tm,
		)
		require.NoError(t, err)
		require.Equal(t, int64(1), tag.RowsAffected())
	}

	hidm := m.G().HostIDMap()

	ins(mainId, 100, start)
	ins(mainId, 101, start.Add(time.Second))
	other := core.ShortHostID(4000)
	ins(other, 200, start.Add(time.Second))

	res, err := hidm.Filter(m, []core.ShortHostID{mainId, 100, 101, 200})
	require.NoError(t, err)
	require.Equal(t, []core.ShortHostID{mainId, 100, 101}, res)

	now := start.Add(time.Hour)

	ins(mainId, 105, now.Add(time.Second*2))
	ins(other, 204, now.Add(time.Second))
	res, err = hidm.Filter(m, []core.ShortHostID{mainId, 100, 101, 105, 200, 204})
	require.NoError(t, err)
	require.Equal(t, []core.ShortHostID{mainId, 100, 101, 105}, res)

	now = now.Add(time.Hour)

	ins(mainId, 110, now)
	res, err = hidm.Filter(m, []core.ShortHostID{mainId, 100, 101, 105, 200, 204, 110, 4000})
	require.NoError(t, err)
	require.Equal(t, []core.ShortHostID{mainId, 100, 101, 105, 110}, res)

	res, err = hidm.Filter(m, []core.ShortHostID{mainId, 100, 105, 204, 4000})
	require.NoError(t, err)
	require.Equal(t, []core.ShortHostID{mainId, 100, 105}, res)
}

// A cold lookup must never hold one server-config connection while it waits for
// another. TLS handshakes resolve SNI through these lookups, so after a restart
// every reconnecting client takes this path at once; when each held a connection
// and waited for a second, a burst as large as the pool took every connection
// and the handshakes waited on each other forever. Each case leaves exactly one
// connection free and expects the lookup to finish with it.
func TestHostIdMapColdLookupNeedsOneConn(t *testing.T) {
	env := globalTestEnv.Fork(t, common.SetupOpts{})
	defer func() {
		_ = env.ShutdownFn()
	}()
	m := env.MetaContext()
	mainId := m.G().ShortHostID()

	hn, err := m.G().HostIDMap().Hostname(m, mainId)
	require.NoError(t, err)

	part, err := core.RandomBase36String(10)
	require.NoError(t, err)
	alias := proto.Hostname("alias-" + part + ".example.com")
	err = shared.PutAlias(m, nil, alias)
	require.NoError(t, err)
	unknown := proto.Hostname("unknown-" + part + ".example.com")

	cfg, err := m.G().Config().DbConfig(m.Ctx(), shared.DbTypeServerConfig)
	require.NoError(t, err)

	tests := []struct {
		name   string
		lookup func(shared.MetaContext, *shared.HostIDMap) (*core.HostID, error)
	}{
		{"hostname", func(m shared.MetaContext, h *shared.HostIDMap) (*core.HostID, error) {
			return h.LookupByHostname(m, hn)
		}},
		{"unknown hostname falls back to the main host", func(m shared.MetaContext, h *shared.HostIDMap) (*core.HostID, error) {
			return h.LookupByHostname(m, unknown)
		}},
		{"alias", func(m shared.MetaContext, h *shared.HostIDMap) (*core.HostID, error) {
			return h.LookupByAlias(m, alias)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i := int32(0); i < cfg.MaxConns-1; i++ {
				db, err := m.Db(shared.DbTypeServerConfig)
				require.NoError(t, err)
				// Closed, not released: the env never closes its pools, so
				// released connections would stay open for the rest of the
				// suite and starve the shared Postgres of connection slots.
				defer func() {
					_ = db.Hijack().Close(context.Background())
				}()
			}
			mt, cancel := m.WithContextTimeout(5 * time.Second)
			defer cancel()
			res, err := tc.lookup(mt, shared.NewHostIDMap())
			require.NoError(t, err)
			require.Equal(t, mainId, res.Short)
		})
	}
}
