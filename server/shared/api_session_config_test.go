// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/stretchr/testify/require"
)

func TestApiSessionDurationConfig(t *testing.T) {
	var nilCfg *WebConfigJSON
	d, err := nilCfg.ApiSessionDuration()
	require.NoError(t, err)
	require.Equal(t, 24*time.Hour, d)

	d, err = (&WebConfigJSON{}).ApiSessionDuration()
	require.NoError(t, err)
	require.Equal(t, 24*time.Hour, d)

	d, err = (&WebConfigJSON{ApiSessionDuration_: core.Duration{Duration: time.Hour}}).ApiSessionDuration()
	require.NoError(t, err)
	require.Equal(t, time.Hour, d)

	// A negative lifetime would mint tokens born expired, and wrap to a huge
	// unsigned DurationMilli on the wire: refuse it.
	_, err = (&WebConfigJSON{ApiSessionDuration_: core.Duration{Duration: -time.Hour}}).ApiSessionDuration()
	require.Error(t, err)
}
