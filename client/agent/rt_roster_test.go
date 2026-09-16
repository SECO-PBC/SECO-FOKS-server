package agent

import (
	"testing"

	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func rosterWith(t *testing.T, names ...string) (*lcl.TeamRoster, map[string]proto.UID) {
	t.Helper()
	uids := map[string]proto.UID{}
	ret := &lcl.TeamRoster{}
	for i, nm := range names {
		var uid proto.UID
		uid[0] = 0x01
		for j := 1; j < len(uid); j++ {
			uid[j] = byte(i + 1)
		}
		uids[nm] = uid
		ret.Members = append(ret.Members, lcl.TeamRosterMember{
			Mem: lcl.NamedFQParty{
				Fqp:  proto.FQParty{Party: uid.ToPartyID()},
				Name: proto.NameUtf8(nm),
				Host: proto.Hostname("h.example"),
			},
		})
	}
	return ret, uids
}

// rtRosterUID maps `name@host` (or a bare name) to the roster member's UID,
// case-insensitively — the resolution the ACL agent RPCs rely on.
func TestRtRosterUID(t *testing.T) {
	roster, uids := rosterWith(t, "uMoss", "uCaro")

	uid, err := rtRosterUID(roster, "uMoss@h.example")
	require.NoError(t, err)
	require.True(t, uid.Eq(uids["uMoss"]))

	// Bare name and case-insensitive both resolve.
	uid, err = rtRosterUID(roster, "ucaro")
	require.NoError(t, err)
	require.True(t, uid.Eq(uids["uCaro"]))

	// Not on the roster → refused, so a grant can never reach the server
	// with an unresolved user.
	_, err = rtRosterUID(roster, "stranger@h.example")
	require.Error(t, err)

	_, err = rtRosterUID(roster, "@h.example")
	require.Error(t, err)
}
