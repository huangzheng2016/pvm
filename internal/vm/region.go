package vm

import (
	"fmt"
	"regexp"
	"strings"
)

// Region profile values end up inside guest shell commands, so keep the
// accepted shapes strict instead of escaping.

var (
	// America/Los_Angeles, Etc/UTC, Asia/Shanghai; bare UTC is also a real
	// zoneinfo file and is what an unconfigured guest reports.
	tzRe = regexp.MustCompile(`^([A-Za-z]+/[A-Za-z_]+(/[A-Za-z_]+)?|UTC)$`)
	// en_US.UTF-8 (normalized form)
	localeRe = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z]{2})?\.UTF-8$`)
)

func ValidateTZ(tz string) error {
	if !tzRe.MatchString(tz) {
		return fmt.Errorf("invalid timezone %q (expected e.g. America/Los_Angeles)", tz)
	}
	return nil
}

func NormalizeLocale(l string) (string, error) {
	l = strings.TrimSpace(l)
	if l == "" {
		return "", fmt.Errorf("empty locale")
	}
	l = strings.ReplaceAll(l, ".utf8", ".UTF-8")
	l = strings.ReplaceAll(l, ".utf-8", ".UTF-8")
	if !strings.Contains(l, ".") {
		l += ".UTF-8"
	}
	if !localeRe.MatchString(l) {
		return "", fmt.Errorf("invalid locale %q (expected e.g. en_US.UTF-8)", l)
	}
	return l, nil
}
