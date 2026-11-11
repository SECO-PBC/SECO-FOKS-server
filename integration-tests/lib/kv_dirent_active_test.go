// Overwriting a KV entry must deactivate only its previous version.
//
// A put inserts a new dirent version and deactivates the older ones. That
// UPDATE once matched every older version, already-inactive ones included,
// so each put rewrote the entry's whole history: O(history) per put, and
// 684ms on staging for an entry with 92k versions. Postgres stamps every
// rewritten row with a new xmin, so an old version's xmin staying put across
// later overwrites is direct evidence they no longer touch it.
package lib

import (
	"fmt"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

func TestKVOverwriteDeactivatesOnlyPreviousVersion(t *testing.T) {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	mc := libkv.NewMetaContext(tew.NewClientMetaContext(t, u))
	kvm := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), libclient.CacheSettings{})

	nm, err := core.RandomDomain()
	require.NoError(t, err)
	path := proto.KVPath("/" + nm + "/hot.json")

	m := tew.MetaContext()
	kvdb, err := m.KVShard(u.uid.ToPartyID())
	require.NoError(t, err)
	defer kvdb.Release()
	host := int(m.ShortHostID())
	party := u.uid.ToPartyID().Shorten().ExportToDB()

	// The hot entry is the one dirent of this user with more than one version.
	type row struct {
		version int
		active  bool
		xmin    string
	}
	versions := func() []row {
		rows, err := kvdb.Query(m.Ctx(),
			`SELECT version, active, xmin::text FROM dirent
			  WHERE short_host_id=$1 AND short_party_id=$2
			    AND dirent_id = (
			      SELECT dirent_id FROM dirent
			       WHERE short_host_id=$1 AND short_party_id=$2
			       GROUP BY dirent_id ORDER BY count(*) DESC LIMIT 1)
			  ORDER BY version`, host, party)
		require.NoError(t, err)
		defer rows.Close()
		var ret []row
		for rows.Next() {
			var r row
			require.NoError(t, rows.Scan(&r.version, &r.active, &r.xmin))
			ret = append(ret, r)
		}
		require.NoError(t, rows.Err())
		return ret
	}

	makeSmallFile(t, mc, kvm, path, "v0")
	overwriteSmallFile(t, mc, kvm, path, "v1")

	// After the first overwrite, version 1 is inactive. Remember which
	// transaction last wrote it.
	vs := versions()
	require.Len(t, vs, 2)
	require.False(t, vs[0].active)
	firstXmin := vs[0].xmin

	const nOverwrites = 20
	for i := 2; i <= nOverwrites; i++ {
		overwriteSmallFile(t, mc, kvm, path, fmt.Sprintf("v%d", i))
	}
	last := fmt.Sprintf("v%d", nOverwrites)
	readFile(t, kvm, mc, path, []byte(last))

	vs = versions()
	require.Len(t, vs, nOverwrites+1)
	var active []int
	for _, r := range vs {
		if r.active {
			active = append(active, r.version)
		}
	}
	require.Equal(t, []int{vs[len(vs)-1].version}, active, "exactly the newest version is active")
	require.Equal(t, firstXmin, vs[0].xmin,
		"later overwrites must not rewrite an already-inactive version")

	// Sanity: listing still sees the entry once, at its newest content.
	ls, err := kvm.List(mc, lcl.KVConfig{}, proto.KVPath("/"+nm), nil, rem.KVListOpts{Num: 10})
	require.NoError(t, err)
	require.Len(t, ls.Ents, 1)
}

// Several versions of one entry can be active at once: a put that skips a
// version number, or concurrent puts under READ COMMITTED. The deactivation
// must retire all of them, not just the previous version, which is why it
// keeps the range form (version < new) alongside active=true.
func TestKVOverwriteDeactivatesEveryOlderActiveVersion(t *testing.T) {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	mc := libkv.NewMetaContext(tew.NewClientMetaContext(t, u))
	kvm := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), libclient.CacheSettings{})

	nm, err := core.RandomDomain()
	require.NoError(t, err)
	path := proto.KVPath("/" + nm + "/multi.json")

	m := tew.MetaContext()
	kvdb, err := m.KVShard(u.uid.ToPartyID())
	require.NoError(t, err)
	defer kvdb.Release()
	host := int(m.ShortHostID())
	party := u.uid.ToPartyID().Shorten().ExportToDB()

	makeSmallFile(t, mc, kvm, path, "v0")
	for i := 1; i <= 3; i++ {
		overwriteSmallFile(t, mc, kvm, path, fmt.Sprintf("v%d", i))
	}

	// The entry now has versions 1-4, with only 4 active. Reactivate 1 and
	// 2 directly, so three versions are active at once.
	tag, err := kvdb.Exec(m.Ctx(),
		`UPDATE dirent SET active=true
		  WHERE short_host_id=$1 AND short_party_id=$2
		    AND dirent_id = (
		      SELECT dirent_id FROM dirent
		       WHERE short_host_id=$1 AND short_party_id=$2
		       GROUP BY dirent_id ORDER BY count(*) DESC LIMIT 1)
		    AND version IN (1, 2)`, host, party)
	require.NoError(t, err)
	require.EqualValues(t, 2, tag.RowsAffected())

	overwriteSmallFile(t, mc, kvm, path, "v4")
	readFile(t, kvm, mc, path, []byte("v4"))

	var activeVersions []int
	rows, err := kvdb.Query(m.Ctx(),
		`SELECT version FROM dirent
		  WHERE short_host_id=$1 AND short_party_id=$2 AND active
		    AND dirent_id = (
		      SELECT dirent_id FROM dirent
		       WHERE short_host_id=$1 AND short_party_id=$2
		       GROUP BY dirent_id ORDER BY count(*) DESC LIMIT 1)
		  ORDER BY version`, host, party)
	require.NoError(t, err)
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		activeVersions = append(activeVersions, v)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.Equal(t, []int{5}, activeVersions, "every older active version is deactivated")
}
