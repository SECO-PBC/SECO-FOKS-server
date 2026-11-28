// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"errors"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/merkle"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

type GenericChainLoader struct {
	BaseChainLoader

	eid proto.EntityID
	eng ChainLoaderSubclass

	existing *lcl.GenericChainState
	raw      rem.GenericChain
	msess    *merkle.Session
	ovglrs   []*core.OpenAndVerifyGenericLinkRes
	ma       *merkle.Agent
	res      *lcl.GenericChainState
}

func NewGenericChainLoader(eid proto.EntityID, eng ChainLoaderSubclass) *GenericChainLoader {
	return &GenericChainLoader{
		eid: eid,
		eng: eng,
	}
}

type KeyBookends struct {
	Provision proto.ProvisionInfo
	Revoke    proto.RevokeInfo
}

type ChainLoaderSubclass interface {
	Fetch(MetaContext, rem.LoadGenericChainArg) (rem.GenericChain, error)
	SeedCommitment() *proto.TreeLocationCommitment
	Type() proto.ChainType
	Scoper() Scoper
	MerkleAgent(m MetaContext) (*merkle.Agent, error)

	LoadState(lcl.GenericChainStatePayload) error
	PlayLink(m MetaContext, l proto.LinkOuter, g proto.GenericLinkPayload) error
	SaveState() (lcl.GenericChainStatePayload, error)

	// Check that the owner with the given key is OK to sign this chain. (nil, nil) return
	// means OK. (non-nil, nil) means that at one point the key was good, but it's now important
	// to do a merkle lookup to make sure that then revoke link captures the signed link.
	BookendSigningKey(m MetaContext, owner proto.FQEntity, key proto.EntityID, epno proto.MerkleEpno) (*KeyBookends, error)
}

func (c *GenericChainLoader) dbType() DbType { return DbTypeHard }

func (c *GenericChainLoader) loadExistingChain(m MetaContext) error {
	if c.testing != nil && c.testing.SkipExistingLoad {
		return nil
	}

	var state lcl.GenericChainState
	typ := c.eng.Type()
	_, err := m.DbGet(&state, c.dbType(), c.eng.Scoper(), lcl.DataType_GenericChainState, typ)
	if errors.Is(err, core.RowNotFoundError{}) {
		return nil
	}
	// Any other read failure is the caller's to see. Carrying on would load
	// the zero state and replay the chain from scratch over a DB that may
	// still hold newer state.
	if err != nil {
		return err
	}
	err = c.eng.LoadState(state.Payload)
	if err != nil {
		return err
	}
	c.existing = &state
	return nil
}

// serverArg is the chain load this loader asks for, given the state it read
// locally. Split out so a batched load can ask for it ahead of time.
func (c *GenericChainLoader) serverArg() rem.LoadGenericChainArg {
	arg := rem.LoadGenericChainArg{
		Eid:   c.eid,
		Typ:   c.eng.Type(),
		Start: proto.ChainEldestSeqno,
	}
	if c.existing != nil {
		arg.Start = c.existing.Tail.Base.Seqno + 1
	}
	return arg
}

func (c *GenericChainLoader) loadChainFromServer(m MetaContext) error {
	ma, err := c.eng.MerkleAgent(m)
	if err != nil {
		return err
	}
	c.ma = ma
	c.msess = merkle.NewSession(ma)
	err = c.msess.Init(m.Ctx())
	if err != nil {
		return err
	}
	res, err := c.eng.Fetch(m, c.serverArg())
	if err != nil {
		return err
	}
	c.raw = res
	return nil
}

func (c *GenericChainLoader) checkMerkleRoot(m MetaContext) error {
	err := c.msess.Run(m.Ctx(), &c.raw.Merkle.Root)
	if err != nil {
		return err
	}
	return nil
}

func (c *GenericChainLoader) openLinks(m MetaContext) error {
	for n, link := range c.raw.Links {
		// Opens and veriies the links, but doesn't check that the signer
		// was valid at the time of the signature.
		ovglr, err := core.OpenAndVerifyGenericLink(link)
		if err != nil {
			return core.ChainLoaderError{
				Err: core.CLOpenLinkError{Err: err, N: n},
			}
		}
		c.ovglrs = append(c.ovglrs, ovglr)
	}
	return nil
}

func (c *GenericChainLoader) chainerAtIndex(n int) *proto.HidingChainer {
	if n >= len(c.ovglrs) {
		return nil
	}
	return &c.ovglrs[n].Link.Chainer
}

func (c *GenericChainLoader) checkChain(m MetaContext) error {
	var prev *proto.LinkHash
	seqno := proto.ChainEldestSeqno
	if c.existing != nil {
		prev = &c.existing.LastHash
		seqno = c.existing.Tail.Base.Seqno + 1
	}
	typ := proto.ChainTypeRevMap[c.eng.Type()]
	return c.BaseChainLoader.checkChain(m, prev, seqno, c.raw.Links, c.chainerAtIndex, typ, nil)
}

func (c *GenericChainLoader) checkMerklePaths(m MetaContext) error {
	seqno := proto.ChainEldestSeqno
	var ntlc *proto.TreeLocationCommitment
	var loc0 *proto.TreeLocation
	if c.existing != nil {
		seqno = c.existing.Tail.Base.Seqno + 1
		ntlc = &c.existing.Tail.NextLocationCommitment
	} else {
		seed := c.raw.LocationSeed
		if seed == nil {
			return core.ChainLoaderError{
				Err: core.CLMissingSubchainTreeLocationSeedError{},
			}
		}
		com := c.eng.SeedCommitment()
		if com == nil {
			return core.InternalError("seed commitment is nil in GenericChainLoader::checkMerklePaths")
		}
		if com.IsZero() {
			return core.InternalError("seed commitment is zero in GenericChainLoader::checkMerklePaths")
		}
		err := core.VerifyTreeLocationCommitment(*seed, *com)
		if err != nil {
			return core.ChainLoaderError{
				Err: err,
			}
		}
		loc0, err = core.SubchainTreeLocation(*seed, c.eng.Type())
		if err != nil {
			return err
		}
	}
	return c.BaseChainLoader.checkMerklePaths(
		m,
		c.eid,
		c.eng.Type(),
		c.raw.Locations,
		c.raw.Links,
		c.chainerAtIndex,
		c.raw.Merkle,
		ntlc,
		seqno,
		loc0,
		0,
		proto.ChainTypeRevMap[c.eng.Type()],
		c.testing,
	)
}

func (c *GenericChainLoader) checkSigningKeys(m MetaContext) error {

	type bookendInfo struct {
		dbe KeyBookends
		i   int
	}

	var checks []bookendInfo

	for i, link := range c.ovglrs {
		owner := link.Link.Entity

		epno := link.Link.Chainer.Base.Root.Epno
		signer := link.Verifier.GetEntityID()
		bookends, err := c.eng.BookendSigningKey(m, owner, signer, epno)
		if err != nil {
			return core.ChainLoaderError{
				Err: core.CLInvalidSignerError{
					Err:   err,
					Seqno: link.Link.Chainer.Base.Seqno,
				},
			}
		}

		// keep track of the keys that have been revoked, we only need to keep track of the
		// newest link signed by the revoked key. Hence this last-writer-wins map here.
		if bookends != nil {
			checks = append(checks, bookendInfo{dbe: *bookends, i: i})
		}
	}

	// no further worked needed if we didn't hit any revokes.
	if len(checks) == 0 {
		return nil
	}

	// now set the root in the merkle session to be the root of the chain we just loaded.
	err := c.msess.InitWithRoot(m.Ctx(), &c.raw.Merkle.Root)
	if err != nil {
		return err
	}

	checkRevokeBookend := func(v bookendInfo) error {
		leaf := proto.MerkleLeaf{
			Key:   c.merkleLeaves[v.i].Key,
			Value: c.linkHashes[v.i].ToStdHash(),
		}
		seqno := c.ovglrs[v.i].Link.Chainer.Base.Seqno
		start := v.dbe.Revoke.Chain.Root
		return CheckMerkleHistoricalInclusion(m, c.ma, c.msess, start, leaf, seqno)
	}

	checkProvisionBookend := func(v bookendInfo) error {
		link := c.ovglrs[v.i].Link
		root := link.Chainer.Base.Root
		leaf := v.dbe.Provision.Leaf
		return CheckMerkleHistoricalInclusion(m, c.ma, c.msess, root, leaf, link.Chainer.Base.Seqno)
	}

	for _, v := range checks {
		err := checkRevokeBookend(v)
		if err != nil {
			return err
		}
		err = checkProvisionBookend(v)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *GenericChainLoader) check(m MetaContext) error {
	err := c.checkMerkleRoot(m)
	if err != nil {
		return err
	}
	err = c.openLinks(m)
	if err != nil {
		return err
	}
	err = c.checkChain(m)
	if err != nil {
		return err
	}
	err = c.checkMerklePaths(m)
	if err != nil {
		return err
	}
	err = c.checkSigningKeys(m)
	if err != nil {
		return err
	}
	return nil
}

func (c *GenericChainLoader) playLinks(m MetaContext) error {
	for i, link := range c.ovglrs {
		err := c.eng.PlayLink(m, c.raw.Links[i], link.Link.Payload)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *GenericChainLoader) saveState(m MetaContext) error {

	l := len(c.raw.Links)

	if l == 0 {
		// It's OK to have an empty sigchain for generic sidechains.
		c.res = c.existing
		return nil
	}

	newState := lcl.GenericChainState{
		LastHash: *c.lastHash,
		Tail:     c.ovglrs[l-1].Link.Chainer,
	}

	tmp, err := c.eng.SaveState()
	if err != nil {
		return err
	}
	newState.Payload = tmp

	err = m.DbPut(c.dbType(), PutArg{
		Scope: c.eng.Scoper(),
		Typ:   lcl.DataType_GenericChainState,
		Key:   c.eng.Type(),
		Val:   &newState,
	})

	if err != nil {
		return err
	}
	c.res = &newState
	return nil
}

func (c *GenericChainLoader) runOnce(m MetaContext) error {

	err := c.loadExistingChain(m)
	if err != nil {
		return err
	}
	err = c.loadChainFromServer(m)
	if err != nil {
		return err
	}
	err = c.check(m)
	if err != nil {
		return err
	}
	err = c.playLinks(m)
	if err != nil {
		return err
	}
	err = c.saveState(m)
	if err != nil {
		return err
	}
	return nil
}

func (c *GenericChainLoader) resetState() {
	c.ovglrs = nil
	c.existing = nil
}

// CheckMerkleHistoricalInclusion formally checks the "happens before" relationship between
// two signatures. We very conservatively say that a signature A happens before a signature B
// if B contains the actual bytes of A (via hash tree inclusion). The historical sequence of
// merkle trees are the mechanism by which this is enforced. We rely further on the property that
// if signature A is in the merkle tree at time T, it will still be there for all times R > T.
func CheckMerkleHistoricalInclusion(
	m MetaContext,
	ma *merkle.Agent,
	msess *merkle.Session,
	start proto.TreeRoot,
	leaf proto.MerkleLeaf,
	seqno proto.Seqno,
) error {
	lres, err := ma.Lookup(m.Ctx(), rem.MerkleLookupArg{
		Key:  leaf.Key,
		Root: &start.Epno,
	})
	if err != nil {
		return core.ChainLoaderError{
			Err: core.CLBadMerkleLookupError{
				Err:   err,
				Seqno: seqno,
			},
		}
	}
	var tmp proto.MerkleRootHash
	err = merkle.HashRoot(&lres.Root, &tmp)
	if err != nil {
		return err
	}

	if !tmp.Eq(&start.Hash) {
		return core.ChainLoaderError{
			Err: core.CLBadMerkleRootHashError{
				Epno:    start.Epno,
				Exected: start.Hash,
				Actual:  tmp,
			},
		}
	}

	err = msess.CheckHistoricalRoot(m.Ctx(), &lres.Root)
	if err != nil {
		return core.ChainLoaderError{
			Err: core.CLBadMerkleHistoricalRootError{
				Err:  err,
				Epno: start.Epno,
			},
		}
	}
	hsh, err := merkle.VerifyPresence(&lres, &leaf.Key)
	if err != nil {
		return core.ChainLoaderError{
			Err: core.CLBadMerkleVerifyPresenceError{
				Err:  err,
				Epno: start.Epno,
				Key:  leaf.Key,
			},
		}
	}
	if !hsh.Eq(leaf.Value) {
		return core.ChainLoaderError{
			Err: core.CLBadMerkleHistoricalLeafValueError{
				Epno: start.Epno,
				Key:  leaf.Key,
			},
		}
	}
	return nil
}

func (c *GenericChainLoader) Run(m MetaContext) error {
	err := c.BaseChainLoader.runMany(m, c.runOnce, c.resetState)
	return err
}
