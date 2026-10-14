package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
	"github.com/huangzheng2016/pvm/internal/vm"
	"github.com/huangzheng2016/pvm/internal/warp"
)

var warpAPIBase string

func exitAddWARP(args []string) int {
	fs := newFlagSet("exit add-warp", "<name> [--tz TZ] [--locale L]")
	tz := fs.String("tz", "", "IANA timezone to apply when a VM uses this exit")
	locale := fs.String("locale", "", "locale to apply when a VM uses this exit")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		if name != "" || fs.NArg() > 1 {
			fs.Usage()
			return 2
		}
		name = fs.Arg(0)
	}
	if name == "" {
		fs.Usage()
		return 2
	}
	if !exit.ValidName(name) {
		return fatalf("invalid exit name %q", name)
	}
	if *tz != "" {
		if err := vm.ValidateTZ(*tz); err != nil {
			return fatalf("%v", err)
		}
	}
	if *locale != "" {
		normalized, err := vm.NormalizeLocale(*locale)
		if err != nil {
			return fatalf("%v", err)
		}
		*locale = normalized
	}
	dir, err := exitsDir()
	if err != nil {
		return fatalf("%v", err)
	}
	if _, err := exit.LoadEntry(dir, name); err == nil {
		return fatalf("exit %q already exists", name)
	}
	privateKey, err := tunnel.GenerateKey()
	if err != nil {
		return fatalf("generate WireGuard key: %v", err)
	}
	publicKey, err := tunnel.PublicKey(privateKey)
	if err != nil {
		return fatalf("derive WireGuard public key: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := warp.NewClient(warpAPIBase)
	fmt.Println("registering a Cloudflare WARP device...")
	registration, err := client.Register(ctx, publicKey)
	if err != nil {
		return fatalf("register WARP: %v", err)
	}
	keep := false
	defer func() {
		if keep {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := client.Delete(cleanupCtx, registration.ID, registration.Token); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to roll back Cloudflare device %s: %v\n", registration.ID, err)
		}
	}()
	entry, err := warp.NewEntry(name, privateKey, registration)
	if err != nil {
		return fatalf("invalid WARP registration: %v", err)
	}
	entry.TZ = *tz
	entry.Locale = *locale
	if err := exit.SaveEntry(dir, entry); err != nil {
		return fatalf("save exit: %v", err)
	}
	keep = true
	fmt.Printf("exit %q added: provider=warp tunnel_ip=%s endpoint=%s mtu=%d\n", name, entry.TunnelIP, entry.Endpoint, entry.MTU)
	fmt.Printf("no VM routes changed; use `pvm route <vm> --via %s` to enable it\n", name)
	return 0
}

func deleteWARPRegistration(e *exit.Entry) error {
	if e.WARP == nil || e.WARP.DeviceID == "" || e.WARP.Token == "" {
		return fmt.Errorf("WARP device credentials are missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return warp.NewClient(warpAPIBase).Delete(ctx, e.WARP.DeviceID, e.WARP.Token)
}
