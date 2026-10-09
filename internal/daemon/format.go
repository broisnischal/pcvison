package daemon

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func bytesH(v uint64) string {
	f := float64(v)
	for _, u := range []string{"B", "K", "M", "G", "T"} {
		if f < 1024 || u == "T" {
			if u == "B" || u == "K" {
				return fmt.Sprintf("%.0f%s", f, u)
			}
			return fmt.Sprintf("%.1f%s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1fT", f)
}

func human(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return human(time.Since(t)) + " ago"
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	r := []rune(s)
	if len(r) > n {
		if n < 4 {
			return string(r[:n])
		}
		return string(r[:n-3]) + "..."
	}
	return s
}

// pct keeps a decimal near the top, where 98.9% and 100% mean different things.
func pct(v float64) string {
	if v >= 95 && v < 100 {
		return fmt.Sprintf("%.1f%%", math.Floor(v*10)/10)
	}
	return fmt.Sprintf("%.0f%%", v)
}
