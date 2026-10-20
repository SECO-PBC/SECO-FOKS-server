// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"context"

	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
)

// Social invites (docs/social_signup_spec.md), inviter side. All calls but
// Reply authorize on the caller being the row's inviter; Reply is made by
// the invitee and carries the write key instead.

func (c *UserClientConn) Create(ctx context.Context, arg rem.CreateArg) error {
	m := shared.NewMetaContextConn(ctx, c)
	return shared.InsertSocialInvite(m, arg, m.UID())
}

func (c *UserClientConn) List(ctx context.Context) ([]rem.SocialInviteRow, error) {
	m := shared.NewMetaContextConn(ctx, c)
	return shared.ListSocialInvites(m, m.UID())
}

func (c *UserClientConn) Reply(ctx context.Context, arg rem.ReplyArg) error {
	m := shared.NewMetaContextConn(ctx, c)
	inviter, err := shared.ReplySocialInvite(m, arg, m.UID())
	if err != nil {
		return err
	}
	// FORK-ONLY (invite-review-push): wake the inviter. Best-effort: the
	// reply is stored, and failing it now would only make the invitee retry
	// into a replacement, which queues nothing.
	if inviter != nil {
		if err := shared.QueueInviteReplyPush(m, *inviter); err != nil {
			m.Warnw("SocialInvite.Reply", "stage", "queue inviter push", "err", err)
		}
	}
	return nil
}

func (c *UserClientConn) AskAgain(ctx context.Context, arg rem.AskAgainArg) error {
	m := shared.NewMetaContextConn(ctx, c)
	return shared.AskAgainSocialInvite(m, arg, m.UID())
}

func (c *UserClientConn) Close(ctx context.Context, arg rem.CloseArg) error {
	m := shared.NewMetaContextConn(ctx, c)
	return shared.CloseSocialInvite(m, arg, m.UID())
}

// NewInviteCode mints a standard single-use code against the caller's
// basket, capped by the caller's outstanding unused codes. Richer allocation
// policy (budgets, per-diem, admin exceptions) is host economics and
// deliberately not built here; see "Left open" in the spec.
func (c *UserClientConn) NewInviteCode(ctx context.Context) (rem.InviteCode, error) {
	m := shared.NewMetaContextConn(ctx, c)
	code, err := shared.NewUserInviteCode(m, m.UID())
	if err != nil {
		return rem.InviteCode{}, err
	}
	return *code, nil
}

var _ rem.SocialInviteInterface = (*UserClientConn)(nil)
