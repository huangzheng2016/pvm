package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	pvmBuildinfo "github.com/huangzheng2016/pvm/internal/buildinfo"
	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
)

const upgradeRepo = "huangzheng2016/pvm"

type ghRelease struct {
	TagName string `json:"tag_name"`
}

func upgradeAsset() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "pvm_darwin_arm64", nil
	case "linux/amd64":
		return "pvm_linux_amd64", nil
	case "windows/amd64":
		return "pvm_windows_amd64", nil
	default:
		return "", fmt.Errorf("unsupported host %s/%s (upgrade manually from releases)", runtime.GOOS, runtime.GOARCH)
	}
}

func latestReleaseTag(client *http.Client) (string, error) {
	resp, err := client.Get("https://api.github.com/repos/" + upgradeRepo + "/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("github api: %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if !releaseTagPattern.MatchString(rel.TagName) {
		return "", fmt.Errorf("invalid tag_name in latest release")
	}
	return rel.TagName, nil
}

func CmdUpgrade(args []string) int {
	fs := newFlagSet("upgrade", "[--check] [--to <tag>] [--rollback] [--no-restart] [-y]")
	rollback := fs.Bool("rollback", false, "restore the saved binary and previously running VMs")
	check := fs.Bool("check", false, "only report current vs latest version")
	to := fs.String("to", "", "upgrade to a specific tag")
	noRestart := fs.Bool("no-restart", false, "replace the binary without restarting the daemon")
	yes := fs.Bool("y", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || (*rollback && (*check || *to != "")) || (*check && *noRestart) {
		return fatalf("incompatible upgrade arguments")
	}
	exe, err := os.Executable()
	if err != nil {
		return fatalf("%v", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fatalf("%v", err)
	}
	if !*check {
		lock, err := fsutil.TryLock(exe + ".upgrade.lock")
		if err != nil {
			return fatalf("lock upgrade: %v", err)
		}
		defer lock.Unlock()
	}
	if *rollback {
		return rollbackUpgrade(exe, *noRestart, *yes)
	}
	asset, err := upgradeAsset()
	if err != nil {
		return fatalf("%v", err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	tag := *to
	if tag == "" {
		tag, err = latestReleaseTag(client)
		if err != nil {
			return fatalf("query latest release: %v", err)
		}
	}
	if !releaseTagPattern.MatchString(tag) {
		return fatalf("invalid release tag")
	}
	cur := pvmBuildinfo.Version
	fmt.Printf("current: %s, target: %s\n", cur, tag)
	if *check {
		if cur == tag {
			fmt.Println("already up to date")
		}
		return 0
	}
	if cur == tag {
		fmt.Println("already up to date")
		return 0
	}
	if !*yes && !confirmAction(os.Stdin, os.Stdout, fmt.Sprintf("upgrade pvm %s -> %s?", cur, tag)) {
		return fatalf("aborted")
	}

	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s.tar.gz", upgradeRepo, tag, asset)
	fmt.Printf("downloading %s...\n", url)
	tmp, err := os.MkdirTemp("", "pvm-upgrade-*")
	if err != nil {
		return fatalf("%v", err)
	}
	defer os.RemoveAll(tmp)
	arc := filepath.Join(tmp, "pvm.tar.gz")
	if err := downloadFile(client, url, arc); err != nil {
		return fatalf("download: %v", err)
	}
	sumsURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/SHA256SUMS", upgradeRepo, tag)
	sums := filepath.Join(tmp, "SHA256SUMS")
	if err := downloadFile(client, sumsURL, sums); err != nil {
		return fatalf("download sums: %v", err)
	}
	if err := verifyReleaseAttestation(sums, tag); err != nil {
		return fatalf("release provenance: %v", err)
	}
	if err := verifyReleaseAttestation(arc, tag); err != nil {
		return fatalf("release provenance: %v", err)
	}
	if err := verifyArchiveSum(arc, sums, asset+".tar.gz"); err != nil {
		return fatalf("checksum: %v", err)
	}

	binName := "pvm"
	if runtime.GOOS == "windows" {
		binName = "pvm.exe"
	}
	bin := filepath.Join(tmp, binName)
	if err := untarSingle(arc, binName, bin); err != nil {
		return fatalf("extract: %v", err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return fatalf("%v", err)
	}
	if err := validateUpgradeBinary(bin, tag); err != nil {
		return fatalf("validate release: %v", err)
	}
	data, err := os.ReadFile(bin)
	if err != nil || len(data) == 0 {
		return fatalf("read extracted binary: %v (refusing to replace %s)", err, exe)
	}
	running, daemonRunning, err := upgradeRunningVMs()
	if err != nil {
		return fatalf("inspect daemon before upgrade: %v", err)
	}
	if daemonRunning && !*noRestart {
		fmt.Printf("daemon will restart; %d running user VM(s) will be restored\n", len(running))
	}
	state := upgradeRecovery{Version: cur, Running: running, DaemonRunning: daemonRunning}
	if err := saveUpgradeRecovery(exe, state); err != nil {
		return fatalf("save rollback: %v", err)
	}
	if err := fsutil.AtomicWrite(exe, data, 0o755); err != nil {
		return fatalf("replace %s: %v", exe, err)
	}
	if *noRestart {
		fmt.Printf("upgraded to %s; restart the daemon to use it: pvm daemon restart; rollback: pvm upgrade --rollback -y\n", tag)
		return 0
	}
	if !daemonRunning {
		fmt.Printf("upgraded to %s; daemon was not running; rollback: pvm upgrade --rollback -y\n", tag)
		return 0
	}
	if err := restoreUpgradeVMs(running, func() int { return CmdDaemon([]string{"restart"}) }, func(name string) int { return CmdStart([]string{name}) }); err != nil {
		return fatalf("upgraded to %s but %v; run `pvm upgrade --rollback -y` to restore the saved binary and VM list", tag, err)
	}
	fmt.Printf("upgraded to %s; daemon restarted and running VMs restored; rollback: pvm upgrade --rollback -y\n", tag)
	return 0
}

var releaseTagPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)

type upgradeRecovery struct {
	Version       string   `json:"version"`
	Running       []string `json:"running"`
	DaemonRunning bool     `json:"daemon_running"`
	SHA256        string   `json:"sha256"`
}

func verifyReleaseAttestation(path, tag string) error {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("GitHub CLI (gh) with attestation support is required; install it before upgrading")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "attestation", "verify", path, "--repo", upgradeRepo,
		"--signer-workflow", upgradeRepo+"/.github/workflows/build.yml", "--source-ref", "refs/tags/"+tag,
		"--deny-self-hosted-runners", "--hostname", "github.com")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("attestation verification failed for %s: %w; check gh authentication and release provenance", filepath.Base(path), err)
	}
	return nil
}

func validateUpgradeBinary(path, version string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("not a Go executable: %w", err)
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH {
		return fmt.Errorf("release platform mismatch (want %s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return fmt.Errorf("release version command: %w", err)
	}
	if strings.TrimSpace(string(out)) != version {
		return fmt.Errorf("release version does not match requested tag")
	}
	return nil
}

func saveUpgradeRecovery(exe string, state upgradeRecovery) error {
	old, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(old)
	state.SHA256 = hex.EncodeToString(sum[:])
	backup := exe + ".previous-" + state.SHA256
	if err := fsutil.AtomicWrite(backup, old, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(exe+".upgrade-"+state.SHA256+".json", data, 0o600); err != nil {
		return fmt.Errorf("save recovery snapshot: %w (original binary unchanged)", err)
	}
	if err := fsutil.AtomicWrite(exe+".upgrade.json", data, 0o600); err != nil {
		return fmt.Errorf("save recovery metadata: %w (original binary unchanged)", err)
	}
	return nil
}

func restoreUpgradeVMs(names []string, restart func() int, start func(string) int) error {
	if restart() != 0 {
		return fmt.Errorf("daemon restart failed; VM recovery list is preserved")
	}
	var failed []string
	for _, name := range names {
		if start(name) != 0 {
			failed = append(failed, name)
		}
	}
	if len(failed) != 0 {
		return fmt.Errorf("VM restore failed for %s (all requested VMs were attempted)", strings.Join(failed, ", "))
	}
	return nil
}

func rollbackUpgrade(exe string, noRestart, yes bool) int {
	raw, err := os.ReadFile(exe + ".upgrade.json")
	if err != nil {
		return fatalf("read rollback metadata: %v", err)
	}
	var state upgradeRecovery
	if err := json.Unmarshal(raw, &state); err != nil {
		return fatalf("read rollback metadata: %v", err)
	}
	if digest, err := hex.DecodeString(state.SHA256); err != nil || len(digest) != sha256.Size {
		return fatalf("invalid rollback checksum")
	}
	backup := exe + ".previous-" + state.SHA256
	old, err := os.ReadFile(backup)
	if err != nil {
		return fatalf("read saved binary: %v", err)
	}
	sum := sha256.Sum256(old)
	if hex.EncodeToString(sum[:]) != state.SHA256 {
		return fatalf("saved binary checksum mismatch")
	}
	if err := validateUpgradeBinary(backup, state.Version); err != nil {
		return fatalf("validate saved binary: %v", err)
	}
	if !yes && !confirmAction(os.Stdin, os.Stdout, "restore saved pvm binary and VM state?") {
		return fatalf("aborted")
	}
	if err := fsutil.AtomicWrite(exe, old, 0o755); err != nil {
		return fatalf("restore binary: %v", err)
	}
	if !noRestart && state.DaemonRunning {
		if err := restoreUpgradeVMs(state.Running, func() int { return CmdDaemon([]string{"restart"}) }, func(name string) int { return CmdStart([]string{name}) }); err != nil {
			return fatalf("binary restored but %v; retry `pvm upgrade --rollback -y`", err)
		}
	}
	if noRestart {
		fmt.Printf("restored %s; restart daemon: pvm daemon restart; recovery files retained\n", state.Version)
	} else {
		fmt.Printf("restored %s; recovery files retained\n", state.Version)
	}
	return 0
}

func upgradeRunningVMs() ([]string, bool, error) {
	p, err := paths.Default()
	if err != nil {
		return nil, false, err
	}
	c := daemon.NewClient(p)
	if !c.Healthy() {
		if _, err := os.Stat(p.DaemonRunLock()); err == nil {
			lock, err := fsutil.TryLock(p.DaemonRunLock())
			if err != nil {
				return nil, true, fmt.Errorf("daemon is running but unresponsive; recover it before upgrading")
			}
			lock.Unlock()
		} else if !os.IsNotExist(err) {
			return nil, false, err
		}
		return nil, false, nil
	}
	var infos []vmInfoView
	if err := c.Get("/v1/vms", &infos); err != nil {
		return nil, true, err
	}
	var running []string
	for _, info := range infos {
		if info.Running && info.Spec.Role != "gateway" {
			running = append(running, info.Spec.Name)
		}
	}
	return running, true, nil
}

func downloadFile(client *http.Client, url, dst string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func verifyArchiveSum(arc, sumsFile, name string) error {
	data, err := os.ReadFile(sumsFile)
	if err != nil {
		return err
	}
	var want string
	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			want = f[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no checksum entry for %s", name)
	}
	raw, err := os.ReadFile(arc)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("sha256 mismatch for %s", name)
	}
	return nil
}

func untarSingle(arcPath, wantName, dst string) error {
	f, err := os.Open(arcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s not found in archive", wantName)
		}
		if err != nil {
			return err
		}
		if hdr.Name != wantName {
			continue
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size <= 0 {
			return fmt.Errorf("invalid executable archive entry")
		}
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
}
