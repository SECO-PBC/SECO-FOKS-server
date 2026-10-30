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

func TestApplyJitterZero(t *testing.T) {
	bg := NewBgTiming(time.Minute*13, time.Second*10, 0)
	for i := 0; i < 100; i++ {
		require.Equal(t, time.Minute*13, bg.Sleep())
		require.Equal(t, time.Second*10, bg.Pause())
	}
}

func TestApplyJitterStaysInPercentBand(t *testing.T) {
	base := time.Minute * 13
	bg := NewBgTiming(base, time.Second*10, 10)

	lo := base - base/10
	hi := base + base/10

	var sawJitter bool
	for i := 0; i < 10000; i++ {
		d := bg.Sleep()
		require.GreaterOrEqual(t, d, lo)
		require.LessOrEqual(t, d, hi)
		if d != base {
			sawJitter = true
		}
	}

	// The whole point of the setting: a fleet of clients must not tick in
	// lockstep. An implementation that truncates the jitter factor to 0
	// returns base every time and passes the bounds check above.
	require.True(t, sawJitter, "jitter=10 never perturbed the duration")
}

func TestApplyJitterCoversBothDirections(t *testing.T) {
	base := time.Minute * 13
	bg := NewBgTiming(base, time.Second*10, 25)

	var sawShorter, sawLonger bool
	for i := 0; i < 10000 && (!sawShorter || !sawLonger); i++ {
		switch d := bg.Sleep(); {
		case d < base:
			sawShorter = true
		case d > base:
			sawLonger = true
		}
	}
	require.True(t, sawShorter, "jitter never shortened the duration")
	require.True(t, sawLonger, "jitter never lengthened the duration")
}

func TestApplyJitterLargeValuesStayBounded(t *testing.T) {
	bases := []time.Duration{
		time.Nanosecond,
		time.Millisecond,
		time.Second * 10,
		time.Minute * 17,
		time.Hour * 24,

		// Past this point base*pct leaves int64 range for a large jitter,
		// and a wrapped product lands negative about half the time.
		time.Hour * 48,
		time.Hour * 24 * 7,
	}
	jitters := []uint16{100, 101, 1000, 32767, 32768, 65535}

	for _, jit := range jitters {
		for _, base := range bases {
			bg := NewBgTiming(base, base, jit)
			for i := 0; i < 1000; i++ {
				for _, d := range []time.Duration{bg.Sleep(), bg.Pause()} {
					// Positive on its own is too weak: an overflowed
					// product clamps to 1ns and passes. The jitter is
					// capped at 100%, so nothing may exceed 2*base
					// either.
					require.Positive(t, d,
						"jitter=%d base=%s produced a non-positive duration", jit, base)
					require.LessOrEqual(t, d, 2*base,
						"jitter=%d base=%s exceeded 2*base", jit, base)
				}
			}
		}
	}
}
