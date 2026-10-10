package vm

import (
	"fmt"
	"regexp"
	"strings"
)

// Values below end up inside guest shell commands or cloud-init
// directives, so keep the accepted shapes strict (same discipline as
// region.go's TZ/Locale rules).
var (
	userRe      = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	guestPathRe = regexp.MustCompile(`^/[a-zA-Z0-9/_.-]+$`)
	gitUnsafeRe = regexp.MustCompile("[$`\\\\\n\r]")
)

// ValidateSpec rejects spec/mount fields that would inject into guest
// shell (runcmd, ssh argv) or break cloud-init.
func ValidateSpec(spec *Spec, mounts []GuestMount) error {
	if !userRe.MatchString(spec.User) {
		return fmt.Errorf("invalid user %q: must match %s", spec.User, userRe)
	}
	for _, m := range mounts {
		if !guestPathRe.MatchString(m.Guest) || strings.Contains(m.Guest, "..") {
			return fmt.Errorf("invalid guest mount path %q", m.Guest)
		}
	}
	if gitUnsafeRe.MatchString(spec.GitEmail) {
		return fmt.Errorf("invalid git email %q: no $, backquote, backslash or newlines", spec.GitEmail)
	}
	if gitUnsafeRe.MatchString(spec.GitName) {
		return fmt.Errorf("invalid git name %q: no $, backquote, backslash or newlines", spec.GitName)
	}
	return nil
}
