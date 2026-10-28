package chain

import "testing"

func TestForkStatusWarnings(t *testing.T) {
	// Lane order is [C, G]; the two numbers are how many blocks of each lane in the window carry
	// the bit. The denominator is Window.
	cases := []struct {
		name     string
		window   int
		forks    []ForkState
		wantLen  int
		wantSev  string
		wantCode string
	}{
		{
			name:    "young chain, nothing counted",
			window:  0,
			forks:   []ForkState{{Fork: 7, Signalling: [2]int{5, 5}}},
			wantLen: 0,
		},
		{
			name:    "a known fork, heavily signalled, is not a warning",
			window:  100,
			forks:   []ForkState{{Fork: ForkLaneDifficulty, Known: true, Signalling: [2]int{50, 50}}},
			wantLen: 0,
		},
		{
			name:    "an unknown fork below the notice threshold is silent",
			window:  100,
			forks:   []ForkState{{Fork: 9, Signalling: [2]int{3, 2}}}, // 5%
			wantLen: 0,
		},
		{
			name:     "an unknown fork at a fifth of the blocks is a notice",
			window:   100,
			forks:    []ForkState{{Fork: 9, Signalling: [2]int{12, 8}}}, // 20%
			wantLen:  1,
			wantSev:  "notice",
			wantCode: "unknown-fork",
		},
		{
			name:     "an unknown fork at most of the blocks is a warning",
			window:   100,
			forks:    []ForkState{{Fork: 9, Signalling: [2]int{40, 40}}}, // 80%
			wantLen:  1,
			wantSev:  "warning",
			wantCode: "unknown-fork",
		},
		{
			name:   "an unknown fork with no blocks signalling is silent",
			window: 100,
			forks:  []ForkState{{Fork: 9, Signalling: [2]int{0, 0}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := ForkStatus{Window: c.window, Forks: c.forks}
			got := st.Warnings()
			if len(got) != c.wantLen {
				t.Fatalf("got %d warnings, want %d: %+v", len(got), c.wantLen, got)
			}
			if c.wantLen == 1 {
				if got[0].Severity != c.wantSev {
					t.Errorf("severity = %q, want %q", got[0].Severity, c.wantSev)
				}
				if got[0].Code != c.wantCode {
					t.Errorf("code = %q, want %q", got[0].Code, c.wantCode)
				}
				if got[0].Message == "" {
					t.Error("message is empty")
				}
			}
		})
	}
}

// A minority that mines must not be able to raise a warning that would scare or (if a stop were
// ever wired to it) halt other nodes: the warning threshold sits well above any single lane's
// guaranteed share of the blocks.
func TestForkStatusWarningsMinorityCannotWarn(t *testing.T) {
	// One whole lane (half the window) signals an unknown bit — still below the warning threshold.
	st := ForkStatus{Window: 100, Forks: []ForkState{{Fork: 9, Signalling: [2]int{50, 0}}}}
	for _, w := range st.Warnings() {
		if w.Severity == "warning" {
			t.Fatalf("a single lane (50%%) raised a warning: %+v", w)
		}
	}
}
