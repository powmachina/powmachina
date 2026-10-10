package explorer

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// cogsPerCoin is 10^8 — one POWM. The division is integer, the result is assembled as text.
// A conversion through a float would lose precision on large amounts — balances that do not match.
const cogsPerCoin = 100_000_000

// FormatCogs prints an amount in POWM with eight decimal places and separated thousands.
func FormatCogs(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	whole, frac := v/cogsPerCoin, v%cogsPerCoin
	s := groupThousands(whole) + "." + fmt.Sprintf("%08d", frac)
	if neg {
		s = "−" + s
	}
	return s
}

// FormatCogsShort trims trailing zeros — for lists where eight decimal places are just noise.
func FormatCogsShort(v int64) string {
	s := FormatCogs(v)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",") // the English notation: 1,000,000
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// FormatHashrate assembles the unit from an integer. The hashrate arrives as a decimal string, because
// at a higher difficulty it overflows an int64 — in an earlier explorer it overflowed a NUMERIC(36,18).
func FormatHashrate(s string) string {
	if s == "" || s == "0" {
		return "—"
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return s
	}
	units := []string{"H/s", "kH/s", "MH/s", "GH/s", "TH/s", "PH/s", "EH/s"}
	thousand := big.NewInt(1000)
	idx := 0
	rem := big.NewInt(0)
	for idx < len(units)-1 && n.Cmp(thousand) >= 0 {
		n.QuoRem(n, thousand, rem)
		idx++
	}
	if idx == 0 {
		return n.String() + " " + units[0]
	}
	// One decimal place from the remainder, without floats.
	return fmt.Sprintf("%s.%d %s", n, rem.Int64()/100, units[idx])
}

// FormatBytes for block and mempool sizes.
func FormatBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d.%d kB", n/1024, (n%1024)*10/1024)
	default:
		return fmt.Sprintf("%d.%d MB", n/(1024*1024), (n%(1024*1024))*10/(1024*1024))
	}
}

// FormatTime is UTC and ISO. The server's local time would turn the same page into different pages for
// different people.
func FormatTime(unix int64) string {
	if unix == 0 {
		return "—"
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05 UTC")
}

// Since is "4 min ago". It is computed on the server at render time, so it is the time the page was
// issued, not the browser's time — and therefore it **does not update itself**. That is how it should
// be: a blinking counter is decoration, not information.
func Since(unix int64, now time.Time) string {
	if unix == 0 {
		return ""
	}
	d := now.Sub(time.Unix(unix, 0))
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}

// Permille prints per mille as a percentage with one decimal place, without floats.
func Permille(p int64) string {
	return fmt.Sprintf("%d.%d%%", p/10, p%10)
}

// Short shortens a hash to its first and last 8 characters. The full hash is in the link, the shortened
// one is what can be read with the eyes.
func Short(h string) string {
	if len(h) <= 20 {
		return h
	}
	return h[:8] + "…" + h[len(h)-8:]
}

// Bar is a textual bar for a lane's share. No JS, no SVG — the width is computed on the server.
func Bar(permille int64, width int) string {
	if width <= 0 {
		width = 24
	}
	filled := int(permille) * width / 1000
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("·", width-filled)
}

// FormatDuration is a time range in the chart: "6 hours", "35 min", "2 days".
func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", max(int(d.Minutes()), 1))
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// AlgoName is the name of the algorithm a block was mined with. In the UI the algorithm is always
// called this and nothing else — no "CPU", no "RandomX v2" in brackets. What is under the hood belongs
// in the documentation and in NOTICE, not on every screen.
func AlgoName(lane string) string {
	switch lane {
	case "C":
		return "MachinaHash-C"
	case "G":
		return "MachinaHash-G"
	}
	return lane
}
