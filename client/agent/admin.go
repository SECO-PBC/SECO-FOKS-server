// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package agent

import (
	"context"

	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
)

func (c *AgentConn) WebAdminPanelLink(
	ctx context.Context,
) (
	proto.URLString,
	error,
) {
	var zed proto.URLString
	m := c.MetaContext(ctx)
	au, err := m.ActiveConnectedUser(nil)
	if err != nil {
		return zed, err
	}
	usrv, err := au.UserClient(m)
	if err != nil {
		return zed, err
	}
	ret, err := usrv.NewWebAdminPanelURL(m.Ctx())
	if err != nil {
		return zed, err
	}
	return ret, nil
}

func (c *AgentConn) CheckLink(
	ctx context.Context,
	url proto.URLString,
) error {
	m := c.MetaContext(ctx)
	au, err := m.ActiveConnectedUser(nil)
	if err != nil {
		return err
	}
	usrv, err := au.UserClient(m)
	if err != nil {
		return err
	}
	err = usrv.CheckURL(m.Ctx(), url)
	return err
}

var _ lcl.AdminInterface = (*AgentConn)(nil)
