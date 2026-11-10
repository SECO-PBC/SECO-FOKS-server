// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package kvStore

import (
	"context"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// TestAuthorizeLockedWriteNoActor pins the trust boundary without a database:
// a remote member or a team acting as member yields no actor, and no actor is
// refused, not looked up. Reaching the Sage list here would panic on the nil
// context value, which is the point of failing before it.
func TestAuthorizeLockedWriteNoActor(t *testing.T) {
	m := shared.NewMetaContext(context.Background(), nil)

	var tid proto.TeamID
	tid[0] = byte(proto.EntityType_NamedTeam)
	teamPid := tid.ToPartyID()
	err := authorizeLockedWrite(m, teamPid)
	require.IsType(t, core.KVPermssionError{}, err)

	// An actor with no Sage list on the context fails closed too.
	var uid proto.UID
	uid[0] = byte(proto.EntityType_User)
	err = authorizeLockedWrite(withActor(m, &uid), teamPid)
	require.IsType(t, core.KVPermssionError{}, err)

	// A user party has no system store to write to, whoever asks.
	err = authorizeLockedWrite(withActor(m, &uid), uid.ToPartyID())
	require.IsType(t, core.KVPermssionError{}, err)
}
