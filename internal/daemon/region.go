package daemon

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// applyRegionProfile aligns the guest timezone/locale with the exit
// node's geography when a VM is routed via it. The VM's create-time
// baseline (spec.TZ/Locale) stays untouched; the values found in the
// guest are remembered in PrevTZ/PrevLocale so a later `--via direct`
// can restore them. Returns a human note (empty when nothing to do).
func (s *Server) applyRegionProfile(spec *vm.Spec, via string) string {
	entry, err := exit.LoadEntry(filepath.Join(s.P.Home, "exits"), via)
	if err != nil || (entry.TZ == "" && entry.Locale == "") {
		return ""
	}
	tz, locale := entry.TZ, entry.Locale
	if tz == "" {
		tz = "UTC"
	}
	if locale == "" {
		locale = "en_US.UTF-8"
	}
	if spec.PrevTZ == "" && spec.PrevLocale == "" {
		// First route into a region: remember what the guest had.
		// timedated caches the pre-cloud-init zone for the whole boot,
		// so read the /etc/localtime symlink instead of timedatectl.
		curTZ, err1 := s.sshRun(spec, "readlink /etc/localtime 2>/dev/null | sed 's|.*/zoneinfo/||'")
		curLocale, err2 := s.sshRun(spec, `grep -h '^LANG=' /etc/default/locale 2>/dev/null | cut -d= -f2`)
		if err1 != nil || err2 != nil {
			return fmt.Sprintf("region profile for %q pending (vm unreachable via ssh): tz=%s locale=%s", via, tz, locale)
		}
		spec.PrevTZ = strings.TrimSpace(curTZ)
		spec.PrevLocale = strings.TrimSpace(curLocale)
		if err := spec.Save(s.P); err != nil {
			return fmt.Sprintf("region profile applied in guest but spec save failed: %v", err)
		}
	}
	return s.setGuestRegion(spec, tz, locale)
}

// restoreRegion rolls back a region profile applied by applyRegionProfile.
func (s *Server) restoreRegion(spec *vm.Spec) string {
	if spec.PrevTZ == "" && spec.PrevLocale == "" {
		return ""
	}
	tz, locale := spec.PrevTZ, spec.PrevLocale
	if tz == "" {
		tz = spec.TZ
	}
	if tz == "" {
		tz = "UTC"
	}
	if locale == "" {
		locale = spec.Locale
	}
	if locale == "" {
		locale = "en_US.UTF-8"
	}
	note := s.setGuestRegion(spec, tz, locale)
	spec.PrevTZ, spec.PrevLocale = "", ""
	if err := spec.Save(s.P); err != nil && !strings.HasPrefix(note, "region apply failed") {
		note = fmt.Sprintf("region restored in guest but spec save failed: %v", err)
	}
	return note
}

// setGuestRegion applies timezone+locale inside the guest (NOPASSWD sudo
// is provisioned by cloud-init).
func (s *Server) setGuestRegion(spec *vm.Spec, tz, locale string) string {
	if err := vm.ValidateTZ(tz); err != nil {
		return fmt.Sprintf("skip region apply: %v", err)
	}
	if _, err := vm.NormalizeLocale(locale); err != nil {
		return fmt.Sprintf("skip region apply: %v", err)
	}
	cmds := []string{
		fmt.Sprintf("sudo timedatectl set-timezone %s", tz),
		fmt.Sprintf("sudo locale-gen %s", locale),
		fmt.Sprintf("sudo update-locale LANG=%s", locale),
		// /etc/environment covers sessions that skip PAM locale.
		fmt.Sprintf("grep -q '^LANG=' /etc/environment && sudo sed -i 's/^LANG=.*/LANG=%s/' /etc/environment || echo 'LANG=%s' | sudo tee -a /etc/environment", locale, locale),
	}
	for _, c := range cmds {
		if out, err := s.sshRun(spec, c); err != nil {
			return fmt.Sprintf("region apply failed at %q: %v (%s)", c, err, strings.TrimSpace(out))
		}
	}
	return fmt.Sprintf("region applied: tz=%s locale=%s", tz, locale)
}
