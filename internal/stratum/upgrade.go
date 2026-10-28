package stratum

import (
	"fmt"
	"strconv"
	"strings"
)

// The stratum update-notice.
//
// The node knows the version of the release it is part of (the miner shipped alongside it). When a
// miner logs in announcing an older version in its agent string, the node sends back a one-line
// notice the miner prints. It is a courtesy, not a gate: the miner keeps mining, nothing a reward
// or a block depends on is involved, and a miner that lies about its version only skips a
// reminder. The agent string is whatever the client sent, so it is parsed defensively and never
// echoed back verbatim — the notice carries only the node's own version and a fixed URL.

// minerAgentPrefix is what the official miner puts in front of its version (cmd/powmachina-miner).
const minerAgentPrefix = "powmachina-miner/"

// updateURL is where a miner is pointed for a newer build (the site's download page).
const updateURL = "https://powmachina.com/download/"

type semver struct{ major, minor, patch int }

func (a semver) less(b semver) bool {
	switch {
	case a.major != b.major:
		return a.major < b.major
	case a.minor != b.minor:
		return a.minor < b.minor
	default:
		return a.patch < b.patch
	}
}

// parseSemver reads an "x.y.z" core, ignoring a leading 'v' and any "-pre" or "+build" suffix. It
// returns false for anything that is not three non-negative numbers — including "dev", the version
// an unstamped development build carries.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var v semver
	for i, dst := range []*int{&v.major, &v.minor, &v.patch} {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return semver{}, false
		}
		*dst = n
	}
	return v, true
}

// minerVersion pulls the version out of a miner's agent string, or false if it is not our miner or
// the version is not a clean x.y.z.
func minerVersion(agent string) (semver, bool) {
	if !strings.HasPrefix(agent, minerAgentPrefix) {
		return semver{}, false
	}
	return parseSemver(strings.TrimPrefix(agent, minerAgentPrefix))
}

// upgradeNotice is the line the server sends a miner at login, or "" when there is nothing to say:
// the node's own version is not a release (an unstamped "dev" build), the agent is not a miner this
// node recognises, or the miner is already at or above the node's version.
func upgradeNotice(nodeVersion, agent string) string {
	want, ok := parseSemver(nodeVersion)
	if !ok {
		return ""
	}
	have, ok := minerVersion(agent)
	if !ok || !have.less(want) {
		return ""
	}
	return fmt.Sprintf("a newer PowMachina miner is available (%d.%d.%d) — you can keep mining; update at %s",
		want.major, want.minor, want.patch, updateURL)
}
