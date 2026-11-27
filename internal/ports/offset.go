package ports

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseOffset parses an offset: "-30d", "+20s", "-1h30m", "90m"; d is a day.
func ParseOffset(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	rest := strings.TrimLeft(s, "+-")
	var total time.Duration
	if days, tail, ok := strings.Cut(rest, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("offset %q: expected, for example, -30d or +20s", s)
		}
		total, rest = time.Duration(n)*24*time.Hour, tail
	}
	if rest != "" {
		d, err := time.ParseDuration(rest)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("offset %q: expected, for example, -30d or +20s", s)
		}
		total += d
	}
	if neg {
		total = -total
	}
	return total, nil
}

// FormatOffset writes an offset for the header: "-720h0m0s" or "+20s".
func FormatOffset(d time.Duration) string {
	if d < 0 {
		return d.String()
	}
	return "+" + d.String()
}
