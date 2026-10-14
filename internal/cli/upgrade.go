package cli

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/buildinfo"
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
	if rel.TagName == "" {
		return "", fmt.Errorf("no tag_name in latest release")
	}
	return rel.TagName, nil
}

func CmdUpgrade(args []string) int {
	fs := newFlagSet("upgrade", "[--check] [--to <tag>] [--no-restart] [-y]")
	check := fs.Bool("check", false, "only report current vs latest version")
	to := fs.String("to", "", "upgrade to a specific tag")
	noRestart := fs.Bool("no-restart", false, "replace the binary without restarting the daemon")
	yes := fs.Bool("y", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return 2
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
	cur := buildinfo.Version
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

	var running []string
	daemonRunning := false
	if !*noRestart {
		var err error
		running, daemonRunning, err = upgradeRunningVMs()
		if err != nil {
			return fatalf("inspect daemon before upgrade: %v", err)
		}
		if daemonRunning {
			fmt.Printf("daemon will restart after the upgrade; %d running user VM(s) will be restored\n", len(running))
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return fatalf("%v", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fatalf("%v", err)
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
	if err := verifyArchiveSum(arc, sums, asset+".tar.gz"); err != nil {
		return fatalf("checksum: %v", err)
	}

	bin := filepath.Join(tmp, "pvm")
	if err := untarSingle(arc, "pvm", bin); err != nil {
		return fatalf("extract: %v", err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return fatalf("%v", err)
	}
	data, err := os.ReadFile(bin)
	if err != nil || len(data) == 0 {
		return fatalf("read extracted binary: %v (refusing to replace %s)", err, exe)
	}
	if err := fsutil.AtomicWrite(exe, data, 0o755); err != nil {
		return fatalf("replace %s: %v", exe, err)
	}
	if *noRestart {
		fmt.Printf("upgraded to %s; restart the daemon to use it: pvm daemon restart\n", tag)
		return 0
	}
	if !daemonRunning {
		fmt.Printf("upgraded to %s; daemon was not running\n", tag)
		return 0
	}
	fmt.Println("restarting daemon...")
	if rc := CmdDaemon([]string{"restart"}); rc != 0 {
		return fatalf("binary upgraded to %s but daemon restart failed; previously running VMs were not restored", tag)
	}
	for _, name := range running {
		fmt.Printf("restoring %s...\n", name)
		if rc := CmdStart([]string{name}); rc != 0 {
			return fatalf("binary upgraded to %s but restoring %s failed", tag, name)
		}
	}
	fmt.Printf("upgraded to %s; daemon restarted and running VMs restored\n", tag)
	return 0
}

func upgradeRunningVMs() ([]string, bool, error) {
	p, err := paths.Default()
	if err != nil {
		return nil, false, err
	}
	c := daemon.NewClient(p)
	if !c.Healthy() {
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
