package libclient

import (
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// embeddedConfig is a Config as an embedded client has it after open with no
// config file: flags never registered (all zero), an empty file layer.
func embeddedConfig() *Config {
	return &Config{file: newConfigFile("")}
}

// An embedded client builds its Config without registering flags, so the
// flag field reads 0. That must not pin the walk to one team at a time.
func TestTeamExploreConcurrencyUnregisteredFlag(t *testing.T) {
	t.Setenv(prefixed("TEAM_EXPLORE_CONCURRENCY"), "")
	c := embeddedConfig()
	require.Equal(t, uint64(defTeamExploreConcurrency), c.TeamExploreConcurrency())
}

func TestTeamExploreConcurrencyEnv(t *testing.T) {
	t.Setenv(prefixed("TEAM_EXPLORE_CONCURRENCY"), "3")
	c := embeddedConfig()
	require.Equal(t, uint64(3), c.TeamExploreConcurrency())
}

func TestTeamExploreConcurrencyFlagWinsOverEnv(t *testing.T) {
	t.Setenv(prefixed("TEAM_EXPLORE_CONCURRENCY"), "3")
	c := embeddedConfig()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.Int64Var(&c.fl.team.exploreConcurrency, "team-explore-concurrency", -1, "")
	require.NoError(t, fs.Parse([]string{"--team-explore-concurrency", "2"}))
	require.Equal(t, uint64(2), c.TeamExploreConcurrency())
}
