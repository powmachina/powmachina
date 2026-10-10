package explorer

import (
	"testing"
	"time"
)

// The number and time formats are English — the whole site is in English and a decimal comma next to
// English text would look like a bug.
func TestFormatsAreEnglish(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct{ got, want string }{
		{FormatCogs(123_456_789_012_345), "1,234,567.89012345"},
		{FormatCogs(-40_000_000), "−0.40000000"},
		{FormatCogsShort(40_000_000), "0.4"},
		{FormatCogsShort(100_000_000), "1"},
		{Permille(305), "30.5%"},
		{Permille(1000), "100.0%"},
		{FormatHashrate("1234567"), "1.2 MH/s"},
		{FormatHashrate("999"), "999 H/s"},
		{FormatHashrate("0"), "—"},
		{FormatBytes(218), "218 B"},
		{FormatBytes(1536), "1.5 kB"},
		{Since(now.Unix()-5, now), "5 s ago"},
		{Since(now.Unix()-240, now), "4 min ago"},
		{Since(now.Unix()-3*3600, now), "3 h ago"},
		{Since(now.Unix()-2*86400, now), "2 d ago"},
		{Since(now.Unix()+30, now), "just now"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, expected %q", c.got, c.want)
		}
	}
}
