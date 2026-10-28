package stratum

import (
	"strings"
	"testing"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want semver
	}{
		{"v0.2.4", true, semver{0, 2, 4}},
		{"0.2.4", true, semver{0, 2, 4}},
		{"1.20.300", true, semver{1, 20, 300}},
		{"v1.2.3-rc1", true, semver{1, 2, 3}},
		{"1.2.3+build7", true, semver{1, 2, 3}},
		{"dev", false, semver{}},
		{"", false, semver{}},
		{"1.2", false, semver{}},
		{"1.2.3.4", false, semver{}},
		{"1.2.x", false, semver{}},
		{"-1.2.3", false, semver{}},
	}
	for _, c := range cases {
		got, ok := parseSemver(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseSemver(%q) = %+v,%v; want %+v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSemverLess(t *testing.T) {
	less := []struct{ a, b semver }{
		{semver{0, 2, 3}, semver{0, 2, 4}},
		{semver{0, 2, 9}, semver{0, 3, 0}},
		{semver{0, 9, 9}, semver{1, 0, 0}},
	}
	for _, c := range less {
		if !c.a.less(c.b) {
			t.Errorf("%+v should be < %+v", c.a, c.b)
		}
		if c.b.less(c.a) {
			t.Errorf("%+v should not be < %+v", c.b, c.a)
		}
	}
	eq := semver{0, 2, 4}
	if eq.less(eq) {
		t.Error("a version is not less than itself")
	}
}

func TestUpgradeNotice(t *testing.T) {
	cases := []struct {
		name       string
		nodeVer    string
		agent      string
		wantNotice bool
	}{
		{"older miner is nudged", "v0.2.4", "powmachina-miner/v0.2.1", true},
		{"same version, nothing to say", "v0.2.4", "powmachina-miner/v0.2.4", false},
		{"newer miner, node is behind — no nag", "v0.2.4", "powmachina-miner/v0.2.5", false},
		{"dev node build is off", "dev", "powmachina-miner/v0.2.1", false},
		{"empty node version is off", "", "powmachina-miner/v0.2.1", false},
		{"a miner we do not recognise is left alone", "v0.2.4", "xmrig/6.21.0", false},
		{"an unparsable miner version is left alone", "v0.2.4", "powmachina-miner/dev", false},
		{"no agent at all", "v0.2.4", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := upgradeNotice(c.nodeVer, c.agent)
			if (got != "") != c.wantNotice {
				t.Fatalf("upgradeNotice(%q,%q) = %q; wantNotice=%v", c.nodeVer, c.agent, got, c.wantNotice)
			}
			// The attacker-controlled agent string must never be echoed back.
			if got != "" && c.agent != "" && strings.Contains(got, c.agent) {
				t.Errorf("notice echoed the agent string: %q", got)
			}
		})
	}
}
