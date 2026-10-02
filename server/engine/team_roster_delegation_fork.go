// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/server/shared"
)

// checkDelegatedRemovalsOwnAdds (fork-only) narrows the delegated-removal
// rule for this host's communities: a signer below admin may only remove
// members whose current membership THEY added. The chain rule has already
// limited such a link to adds and removes of members at m/0 or below; this
// compares each removed member's add-link signer with this link's signer.
// A server check, not a chain rule: a host that skipped it would widen the
// rule back to "any plain member", never to anything above m/0.
func (c *teamEditor) checkDelegatedRemovalsOwnAdds(
	m shared.MetaContext,
) error {
	if c.signerPreRole == nil || c.signerPreRole.Typ.IsAdminOrAbove() {
		return nil
	}
	if c.openres == nil || len(c.openres.Sched.Removals) == 0 {
		return nil
	}
	signer := c.openres.Gc.Signer.KeyOwner
	if signer == nil {
		return core.PermissionError("link has no signer key owner")
	}
	for _, mid := range c.openres.Sched.Removals {
		adder, err := shared.LoadAdderOfCurrentMembership(m, c.tx, c.teamID, mid)
		if err != nil {
			return err
		}
		if adder == nil || !adder.EntityID().Eq(signer.Party.EntityID()) {
			return core.PermissionError("a delegated change may only remove members its signer added")
		}
	}
	return nil
}
