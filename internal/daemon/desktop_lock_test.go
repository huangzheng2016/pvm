package daemon

import (
	"strings"
	"testing"
)

func TestDesktopDisableLockScript(t *testing.T) {
	for _, want := range []string{
		"/.config/autostart/xfce4-screensaver.desktop",
		"Hidden=true",
		"/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-screensaver.xml",
		`"enabled" type="bool" value="false"`,
		"pkill -x xfce4-screensaver",
		"pkill -x light-locker",
	} {
		if !strings.Contains(desktopDisableLockScript, want) {
			t.Errorf("lock-disable script missing %q", want)
		}
	}
}
