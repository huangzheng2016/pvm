package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// applyRegionProfile aligns the guest timezone/locale with the exit
// node's geography when a VM is routed via it. The VM's create-time
// baseline (spec.TZ/Locale) stays untouched; the values found in the
// guest are remembered in PrevTZ/PrevLocale so a later `--via direct`
// can restore them. Returns a human note (empty when nothing to do).
func (s *Server) applyRegionProfile(spec *vm.Spec, via string) string {
	dir := filepath.Join(s.P.Home, "exits")
	entry, err := exit.LoadEntry(dir, via)
	if err != nil {
		return ""
	}
	// Manual tz wins; then the cached server-suggested zone; then a
	// live probe (one throwaway enroll, revoked immediately) which is
	// cached back into the entry for next time.
	tz, locale := entry.TZ, entry.Locale
	if tz == "" {
		tz = entry.AutoTZ
	}
	if tz == "" {
		tz = s.probeSuggestedTZ(entry)
		if tz != "" {
			entry.AutoTZ = tz
			if err := exit.SaveEntry(dir, entry); err != nil {
				return fmt.Sprintf("learned auto tz %s but entry save failed: %v", tz, err)
			}
		}
	}
	if tz == "" && locale == "" {
		return ""
	}
	if tz == "" {
		tz = "UTC"
	}
	if locale == "" {
		locale = "en_US.UTF-8"
	}
	if spec.PrevTZ == "" {
		// First route into a region: remember what the guest had.
		// timedated caches the pre-cloud-init zone for the whole boot,
		// so read /etc/localtime instead of timedatectl. Alpine ships it
		// as a regular file, hence the /etc/timezone fallback.
		curTZ, err1 := s.sshRun(spec, `z=$(readlink /etc/localtime 2>/dev/null); z=${z##*/zoneinfo/}; if [ -z "$z" ] && [ -f /etc/timezone ]; then z=$(cat /etc/timezone); fi; echo "$z"`)
		curLocale, err2 := s.sshRun(spec, `grep -E '^(export )?LANG=' /etc/default/locale /etc/profile.d/pvm-locale.sh 2>/dev/null | tail -1 | sed 's/^export //' | cut -d= -f2`)
		if err1 != nil || err2 != nil {
			return fmt.Sprintf("region profile for %q pending (vm unreachable via ssh): tz=%s locale=%s", via, tz, locale)
		}
		tz0 := strings.TrimSpace(curTZ)
		if tz0 == "" {
			tz0 = "UTC"
		}
		loc0 := strings.TrimSpace(curLocale)
		if loc0 == "" {
			loc0 = "en_US.UTF-8"
		}
		if tz0 != tz || loc0 != locale {
			spec.PrevTZ = tz0
			spec.PrevLocale = loc0
			if err := spec.Save(s.P); err != nil {
				return fmt.Sprintf("region profile applied in guest but spec save failed: %v", err)
			}
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
	if strings.HasPrefix(note, "region applied") {
		// Only drop the rollback baseline once the restore really took;
		// a failed restore keeps PrevTZ/PrevLocale so a later --via direct
		// can retry instead of losing the pre-route values.
		spec.PrevTZ, spec.PrevLocale = "", ""
	}
	if err := spec.Save(s.P); err != nil && !strings.HasPrefix(note, "region apply failed") {
		note = fmt.Sprintf("region restored in guest but spec save failed: %v", err)
	}
	return note
}

// probeSuggestedTZ learns the exit's suggested timezone via a throwaway
// enroll (fresh key, revoked right after) so no stale peer is left.
func (s *Server) probeSuggestedTZ(entry *exit.Entry) string {
	priv, err := tunnel.GenerateKey()
	if err != nil {
		return ""
	}
	pub, err := tunnel.PublicKey(priv)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cl := exit.NewClient(entry.Server, entry.Token, entry.Fingerprint)
	resp, err := cl.Enroll(ctx, pub, "pvm-tzprobe", entry.Transport)
	if err != nil {
		return ""
	}
	if resp.TunnelIP != "" {
		_ = cl.DeletePeer(ctx, resp.TunnelIP)
	}
	return resp.SuggestedTZ
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
	var cmds []string
	if vm.IsAlpine(spec) {
		// Alpine uses OpenRC and /etc/profile.d, not systemd.
		cmds = []string{
			fmt.Sprintf("sudo ln -sf /usr/share/zoneinfo/%s /etc/localtime && echo %s | sudo tee /etc/timezone", tz, tz),
			fmt.Sprintf("sudo mkdir -p /etc/profile.d && printf 'export LANG=%s\\n' | sudo tee /etc/profile.d/pvm-locale.sh", locale),
		}
	} else {
		cmds = []string{
			fmt.Sprintf("sudo timedatectl set-timezone %s", tz),
			fmt.Sprintf("sudo locale-gen %s", locale),
			fmt.Sprintf("sudo update-locale LANG=%s", locale),
			// /etc/environment covers sessions that skip PAM locale.
			fmt.Sprintf("grep -q '^LANG=' /etc/environment && sudo sed -i 's/^LANG=.*/LANG=%s/' /etc/environment || echo 'LANG=%s' | sudo tee -a /etc/environment", locale, locale),
		}
	}
	for _, c := range cmds {
		if out, err := s.sshRun(spec, c); err != nil {
			return fmt.Sprintf("region apply failed at %q: %v (%s)", c, err, strings.TrimSpace(out))
		}
	}
	return fmt.Sprintf("region applied: tz=%s locale=%s", tz, locale)
}

// sanitizeTZ takes the first non-empty line of guest-provided output and
// validates it as an IANA timezone, falling back to UTC. Used on any TZ
// string read from a guest before it is embedded into shell commands.
func sanitizeTZ(out string) string {
	tz := ""
	for line := range strings.Lines(out) {
		tz = strings.TrimSpace(line)
		if tz != "" {
			break
		}
	}
	if err := vm.ValidateTZ(tz); err != nil {
		return "UTC"
	}
	return tz
}
