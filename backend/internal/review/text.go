package review

import "unicode/utf8"

// Truncate returns s unchanged when len(s) <= max. Otherwise it cuts s at a
// rune boundary so that the result, marker included, is at most max bytes. A
// marker longer than max yields the marker cut to the same bound.
func Truncate(s string, max int, marker string) string {
	if len(s) <= max {
		return s
	}
	if len(marker) >= max {
		return marker[:max]
	}
	cut := max - len(marker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}
