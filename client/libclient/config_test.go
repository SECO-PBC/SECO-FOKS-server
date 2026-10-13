// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// embeddedConfig is the Config a process gets when it embeds the client and
// never parses a command line, which is how mobile, sidecar and daemon clients
// run. It goes through NewGlobalContext so the test covers what those clients
// actually call. The getters read the config file's data, so it gets an empty one.
func embeddedConfig() *Config {
	cfg := NewGlobalContext().Cfg()
	cfg.file = newConfigFile("")
	return cfg
}

func TestEmbeddedConfigDefaults(t *testing.T) {
	cfg := embeddedConfig()

	require.Equal(t, 20*time.Second, cfg.ProbeTimeout())
	require.Equal(t, 1800*time.Second, cfg.KexTimeout())
	require.Equal(t, uint64(defTeamExploreConcurrency), cfg.TeamExploreConcurrency())
	require.Equal(t, uint64(100), cfg.KVListPageSize())
	require.Equal(t, uint64(1024), cfg.GitRepackThreshhold())
	require.Equal(t, uint64(defRTOutboxMaxPerChannel), cfg.RTOutboxMaxPerChannel())

	mr, err := cfg.MerkleRaceRetryConfig()
	require.NoError(t, err)
	require.Equal(t, 10, mr.NumRetries)

	kv, err := cfg.KvConfig()
	require.NoError(t, err)
	require.Equal(t, 500, kv.CacheRace.NumRetries)

	bg, err := cfg.BgConfig()
	require.NoError(t, err)
	require.Equal(t, uint16(10), bg.User.jitter)
	require.Equal(t, uint16(10), bg.Clkr.jitter)
}

func TestEmbeddedConfigReadsEnv(t *testing.T) {
	t.Setenv(prefixed("TEAM_EXPLORE_CONCURRENCY"), "3")
	t.Setenv(prefixed("PROBE_TIMEOUT"), "5")
	cfg := embeddedConfig()

	require.Equal(t, uint64(3), cfg.TeamExploreConcurrency())
	require.Equal(t, 5*time.Second, cfg.ProbeTimeout())
}

func TestEmbeddedConfigExplicitZero(t *testing.T) {
	cfg := embeddedConfig()
	cfg.TestDisableMerkleRaceRetry()
	mr, err := cfg.MerkleRaceRetryConfig()
	require.NoError(t, err)
	require.Equal(t, 0, mr.NumRetries)
}

func TestConfigZeroGuards(t *testing.T) {
	t.Setenv(prefixed("RT_OUTBOX_MAX_PER_CHANNEL"), "0")
	t.Setenv(prefixed("TEAM_EXPLORE_CONCURRENCY"), "0")
	cfg := embeddedConfig()

	require.Equal(t, uint64(defRTOutboxMaxPerChannel), cfg.RTOutboxMaxPerChannel())
	require.Equal(t, uint64(1), cfg.TeamExploreConcurrency())
}

func TestTeamExploreConcurrencyFlagWinsOverEnv(t *testing.T) {
	t.Setenv(prefixed("TEAM_EXPLORE_CONCURRENCY"), "3")
	c := embeddedConfig()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	c.registerFlags(fs)
	require.NoError(t, fs.Parse([]string{"--team-explore-concurrency", "2"}))
	require.Equal(t, uint64(2), c.TeamExploreConcurrency())
}
