// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"context"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// existingChainEng is just enough of a ChainLoaderSubclass to read local
// state: it records whether LoadState ran.
type existingChainEng struct {
	ChainLoaderSubclass
	loaded bool
}

func (e *existingChainEng) Type() proto.ChainType { return proto.ChainType_TeamMembership }
func (e *existingChainEng) Scoper() Scoper {
	var uid proto.UID
	return &uid
}
func (e *existingChainEng) LoadState(lcl.GenericChainStatePayload) error {
	e.loaded = true
	return nil
}

// A local read that fails for any reason other than "no row" is reported,
// not taken as "nothing stored" (which would replay the chain over a zero
// state).
func TestLoadExistingChainReturnsDbErrors(t *testing.T) {
	m := newRaceTestContext(t, MerkleRaceRetryConfig{})
	ctx, cancel := context.WithCancel(m.Ctx())
	cancel()
	m = NewMetaContext(ctx, m.G())

	eng := &existingChainEng{}
	c := NewGenericChainLoader(proto.EntityID{}, eng)
	err := c.loadExistingChain(m)
	require.Error(t, err)
	require.NotErrorIs(t, err, core.RowNotFoundError{})
	require.False(t, eng.loaded)
	require.Nil(t, c.existing)
}
