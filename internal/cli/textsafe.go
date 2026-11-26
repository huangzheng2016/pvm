package cli

import (
	"strings"
	"unicode"
)

// sanitizeText replaces terminal control characters and invisible/bidi
// formatting characters (Unicode Cf) in guest-supplied names before printing,
// preventing terminal escape injection and Trojan-Source style spoofing.
func sanitizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
}
