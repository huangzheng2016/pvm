package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/workspace"
	"github.com/huangzheng2016/pvm/internal/workspace/guest"
	"github.com/huangzheng2016/pvm/internal/workspace/sftpd"
	"github.com/huangzheng2016/pvm/internal/workspace/threeway"
)

const workspaceUsage = `usage: pvm workspace create <id> <vm> --mode mount|sync|copy [--path DIR] [--read-only] [-y]
       pvm workspace ls
       pvm workspace status <id>
       pvm workspace push <id> [--prefer host|vm | --keep-both]
       pvm workspace pull <id> [--prefer host|vm | --keep-both]
       pvm workspace clone <id> <dest-dir>
       pvm workspace ssh <id> [command...]
       pvm workspace rm <id> [--delete-guest] [-y]`

const (
	largeUpload       = 1 << 30
	mountReadyTimeout = 90 * time.Second
	// maxIgnoreFile caps each guest .gitignore/.pvmignore read into memory.
	maxIgnoreFile = 1 << 20
)

const watchNote = "note: host changes do not raise inotify events in the VM; file watchers need polling, for example CHOKIDAR_USEPOLLING=1"

func CmdWorkspace(args []string) int {
	if len(args) == 0 {
		return fatalf("%s", workspaceUsage)
	}
	rest := args[1:]
	switch args[0] {
	case "create":
		return cmdWorkspaceCreate(rest)
	case "ls", "list":
		return cmdWorkspaceList(rest)
	case "status":
		return cmdWorkspaceStatus(rest)
	case "push":
		return cmdWorkspaceTransfer(rest, threeway.Push)
	case "pull":
		return cmdWorkspaceTransfer(rest, threeway.Pull)
	case "clone":
		return cmdWorkspaceClone(rest)
	case "ssh":
		return cmdWorkspaceSSH(rest)
	case "rm":
		return cmdWorkspaceRm(rest)
	}
	return fatalf("unknown workspace subcommand %q\n%s", args[0], workspaceUsage)
}

// parseInterleaved parses flags that may appear before, between or after
// positionals, including value flags (unlike partitionFlags).
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// wsGuest runs workspace commands in a VM over the same ssh path as
// `pvm ssh`, without the agent-channel fallback: transfers need a clean
// binary stdin and stdout.
type wsGuest struct {
	vm   string
	args []string
	run  func(ctx context.Context, command string, stdin io.Reader, stdout io.Writer) error
}

func getVMInfo(c *daemon.Client, name string) (*vmInfoView, error) {
	var info vmInfoView
	if err := c.Get("/v1/vms/"+url.PathEscape(name), &info); err != nil {
		return nil, err
	}
	if info.Spec.Role == "gateway" {
		return nil, fmt.Errorf("%s is a gateway; workspaces need a user VM", name)
	}
	return &info, nil
}

func requireRunning(info *vmInfoView) error {
	if !info.Running {
		return fmt.Errorf("vm %s is not running; run `pvm start %s`", info.Spec.Name, info.Spec.Name)
	}
	return nil
}

func newWSGuest(p *paths.Paths, info *vmInfoView) (*wsGuest, error) {
	if err := requireRunning(info); err != nil {
		return nil, err
	}
	sshArgs, target := vmSSHArgs(p, info)
	sshArgs = append(sshArgs, "-T", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", target)
	g := &wsGuest{vm: info.Spec.Name, args: sshArgs}
	g.run = g.ssh
	return g, nil
}

func (g *wsGuest) ssh(ctx context.Context, command string, stdin io.Reader, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", append(append([]string(nil), g.args...), command)...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %s", g.vm, msg)
		}
		return fmt.Errorf("%s: ssh: %v", g.vm, err)
	}
	return nil
}

func (g *wsGuest) output(command string, stdin []byte) ([]byte, error) {
	var out bytes.Buffer
	err := g.run(context.Background(), command, bytes.NewReader(stdin), &out)
	return out.Bytes(), err
}

// upload streams produce's output to command. GNU tar accepts an archive
// that ends at an entry boundary, so a failed producer appends a corrupt
// block that makes the guest reject the whole stream.
func (g *wsGuest) upload(command string, produce func(io.Writer) error) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := produce(pw)
		if err != nil {
			pw.Write(bytes.Repeat([]byte{0xff}, 1024))
		}
		pw.CloseWithError(err)
		done <- err
	}()
	runErr := g.run(context.Background(), command, pr, io.Discard)
	pr.CloseWithError(errors.New("guest command exited"))
	if err := <-done; err != nil {
		return err
	}
	return runErr
}

// download feeds the guest's stdout to consume and stops the guest command
// when consume fails.
func (g *wsGuest) download(command string, stdin []byte, consume func(io.Reader) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := g.run(ctx, command, bytes.NewReader(stdin), pw)
		pw.CloseWithError(err)
		done <- err
	}()
	err := consume(pr)
	if err != nil {
		cancel()
	} else {
		io.Copy(io.Discard, pr)
	}
	pr.Close()
	runErr := <-done
	if err != nil {
		return err
	}
	return runErr
}

func nulList(paths []string) []byte {
	var b bytes.Buffer
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte(0)
	}
	return b.Bytes()
}

// list returns every guest path below the workspace, unhashed.
func (g *wsGuest) list(gp string) (workspace.Manifest, []workspace.Skipped, error) {
	cmd, err := guest.ListCommand(gp)
	if err != nil {
		return nil, nil, err
	}
	out, err := g.output(cmd, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("list %s: %w", gp, err)
	}
	m, sk, err := guest.ParseList(out)
	if err != nil {
		return nil, nil, err
	}
	var skipped []workspace.Skipped
	for _, s := range sk {
		skipped = append(skipped, workspace.Skipped{Path: s.Path, Reason: s.Reason})
	}
	return m, skipped, nil
}

// complete drops excluded paths from a guest listing and fills in file
// hashes, rehashing only files whose size or mtime differ from prev.
func (g *wsGuest) complete(gp string, all workspace.Manifest, skipped []workspace.Skipped, m workspace.Matcher, prev workspace.Manifest) (workspace.Manifest, []workspace.Skipped, error) {
	out := workspace.Manifest{}
	var need []string
	for p, e := range all {
		if m != nil && m.Excluded(p, e.Type == workspace.TypeDir) {
			continue
		}
		if e.Type == workspace.TypeFile {
			if old, ok := prev[p]; ok && old.Type == workspace.TypeFile && old.Size == e.Size && old.MTime == e.MTime && old.SHA256 != "" {
				e.SHA256 = old.SHA256
			} else {
				need = append(need, p)
			}
		}
		out[p] = e
	}
	if len(need) > 0 {
		sort.Strings(need)
		cmd, err := guest.HashCommand(gp)
		if err != nil {
			return nil, nil, err
		}
		raw, err := g.output(cmd, nulList(need))
		if err != nil {
			return nil, nil, fmt.Errorf("hash files in %s: %w", gp, err)
		}
		sums, err := guest.ParseHashes(raw)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range need {
			sum, ok := sums[p]
			if !ok {
				return nil, nil, fmt.Errorf("guest did not hash %s; rerun the command", p)
			}
			e := out[p]
			e.SHA256 = sum
			out[p] = e
		}
	}
	var kept []workspace.Skipped
	for _, s := range skipped {
		if m == nil || !m.Excluded(s.Path, false) {
			kept = append(kept, s)
		}
	}
	return out, kept, nil
}

func (g *wsGuest) scan(gp string, m workspace.Matcher, prev workspace.Manifest) (workspace.Manifest, []workspace.Skipped, error) {
	all, skipped, err := g.list(gp)
	if err != nil {
		return nil, nil, err
	}
	return g.complete(gp, all, skipped, m, prev)
}

// matcher builds the ignore rules from the guest's own .gitignore and
// .pvmignore files, for downloads of a tree the host does not have.
func (g *wsGuest) matcher(gp string, all workspace.Manifest) (*workspace.IgnoreMatcher, error) {
	want := map[string]bool{}
	var names []string
	for p, e := range all {
		if b := path.Base(p); e.Type == workspace.TypeFile && (b == ".gitignore" || b == ".pvmignore") {
			want[p] = true
			names = append(names, p)
		}
	}
	if len(names) == 0 {
		return workspace.NewMatcher(nil)
	}
	sort.Strings(names)
	cmd, err := guest.DownloadCommand(gp)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	err = g.download(cmd, nulList(names), func(r io.Reader) error {
		tr := tar.NewReader(r)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if hdr.Typeflag == tar.TypeXGlobalHeader {
				continue
			}
			if hdr.Typeflag != tar.TypeReg || !want[hdr.Name] || files[hdr.Name] != nil {
				return fmt.Errorf("unexpected entry %q while reading ignore files", hdr.Name)
			}
			if hdr.Size > maxIgnoreFile {
				return fmt.Errorf("%s is larger than %d bytes", hdr.Name, maxIgnoreFile)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			files[hdr.Name] = data
		}
	})
	if err != nil {
		return nil, fmt.Errorf("read ignore files from %s: %w", gp, err)
	}
	return workspace.NewMatcher(files)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func printSkipped(title string, sk []workspace.Skipped) {
	if len(sk) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "%s:\n", title)
	for _, s := range sk {
		fmt.Fprintf(os.Stderr, "  %s: %s\n", s.Path, s.Reason)
	}
}

func cmdWorkspaceCreate(args []string) int {
	fs := newFlagSet("workspace create", "<id> <vm> --mode mount|sync|copy [--path DIR] [--read-only] [-y]")
	modeFlag := fs.String("mode", "", "mount, sync or copy (required)")
	pathFlag := fs.String("path", "", "host folder (default: current directory)")
	readOnly := fs.Bool("read-only", false, "mount mode only: the VM cannot change host files")
	yes := fs.Bool("y", false, "skip the confirmation for uploads above 1 GiB")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 2 || *modeFlag == "" {
		fs.Usage()
		return 2
	}
	id, vmName := pos[0], pos[1]
	if err := workspace.ValidID(id); err != nil {
		return fatalf("%v", err)
	}
	mode, err := workspace.ParseMode(*modeFlag)
	if err != nil {
		return fatalf("%v", err)
	}
	if *readOnly && mode != workspace.ModeMount {
		return fatalf("--read-only applies to mount mode only")
	}
	host := *pathFlag
	if host == "" {
		if host, err = os.Getwd(); err != nil {
			return fatalf("%v", err)
		}
	}
	if host, err = filepath.Abs(host); err != nil {
		return fatalf("%v", err)
	}
	if fi, err := os.Stat(host); err != nil {
		return fatalf("%v", err)
	} else if !fi.IsDir() {
		return fatalf("%s is not a directory", host)
	}
	fmt.Printf("host folder: %s\n", host)

	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	store := workspace.NewStore(p)
	lock, err := store.Lock(id)
	if err != nil {
		return fatalf("%v", err)
	}
	defer lock.Unlock()
	if _, err := store.Load(id); err == nil {
		return fatalf("workspace %s already exists; see `pvm workspace ls`", id)
	} else if !errors.Is(err, workspace.ErrNotFound) {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	info, err := getVMInfo(c, vmName)
	if err != nil {
		return fatalf("%v", err)
	}
	ws := &workspace.Workspace{
		ID: id, VM: info.Spec.Name, Mode: mode, HostPath: host,
		GuestPath: workspace.GuestPathFor(id), ReadOnly: *readOnly, Created: time.Now().UTC(),
	}
	if mode == workspace.ModeMount {
		return createMount(p, c, store, info, ws)
	}
	return createUpload(p, store, info, ws, *yes)
}

// prepareGuest checks the guest path is free and creates it, owned by the
// login user.
func prepareGuest(g *wsGuest, gp string) error {
	cmd, err := guest.ExistsNonEmptyCommand(gp)
	if err != nil {
		return err
	}
	out, err := g.output(cmd, nil)
	if err != nil {
		return err
	}
	st, err := guest.ParseState(out)
	if err != nil {
		return err
	}
	if st != guest.StateAbsent && st != guest.StateEmpty {
		return fmt.Errorf("%s:%s already exists and is not an empty directory; remove it or pick another id", g.vm, gp)
	}
	if cmd, err = guest.PrepareCommand(gp); err != nil {
		return err
	}
	out, err = g.output(cmd, nil)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", gp, err)
	}
	os.Stderr.Write(out)
	return nil
}

type wsMountStatus struct {
	State string `json:"state"`
	Error string `json:"error"`
	Since string `json:"since"`
}

func createMount(p *paths.Paths, c *daemon.Client, store *workspace.Store, info *vmInfoView, ws *workspace.Workspace) int {
	// No ignore rules: a mount shares every file in the folder.
	if err := workspace.CheckUploadSource(ws.HostPath, uploadHomes(), p.Home, nil); err != nil {
		return fatalf("%v", err)
	}
	if runtime.GOOS != "windows" {
		ws.Transport = workspace.Transport9p
		// Read-only 9p mounts are attached at boot, so the daemon wants the
		// VM stopped and the guest cannot be prepared now.
		if info.Running && !ws.ReadOnly {
			g, err := newWSGuest(p, info)
			if err != nil {
				return fatalf("%v", err)
			}
			if err := prepareGuest(g, ws.GuestPath); err != nil {
				return fatalf("%v", err)
			}
		}
		body := map[string]any{"host": ws.HostPath, "guest": ws.GuestPath, "profile": "coherent", "read_only": ws.ReadOnly}
		if err := c.Post("/v1/vms/"+url.PathEscape(ws.VM)+"/mounts", body, nil); err != nil {
			return fatalf("mount %s at %s:%s: %v", ws.HostPath, ws.VM, ws.GuestPath, err)
		}
		if err := store.Save(ws); err != nil {
			if rmErr := c.Delete(vmMountPath(ws), nil); rmErr != nil {
				return fatalf("save workspace %s: %v; the mount is still attached, remove it with `pvm mount rm %s %s` (%v)", ws.ID, err, ws.VM, ws.GuestPath, rmErr)
			}
			return fatalf("save workspace %s: %v", ws.ID, err)
		}
		if ws.ReadOnly {
			fmt.Printf("mounted %s -> %s:%s (read-only, attached at the next `pvm start %s`)\n", ws.HostPath, ws.VM, ws.GuestPath, ws.VM)
		} else {
			fmt.Printf("mounted %s -> %s:%s\n", ws.HostPath, ws.VM, ws.GuestPath)
		}
		fmt.Println(watchNote)
		return 0
	}

	ws.Transport = workspace.TransportSFTP
	g, err := newWSGuest(p, info)
	if err != nil {
		return fatalf("%v", err)
	}
	if err := prepareGuest(g, ws.GuestPath); err != nil {
		return fatalf("%v", err)
	}
	fmt.Printf("checking sshfs in %s (installed on first use)\n", ws.VM)
	if _, err := g.output(sftpd.InstallCommand(), nil); err != nil {
		return fatalf("install sshfs: %v", err)
	}
	if err := store.Save(ws); err != nil {
		return fatalf("save workspace %s: %v", ws.ID, err)
	}
	st, err := waitMounted(c, ws.ID, mountReadyTimeout)
	if err != nil {
		var cleanup []string
		if rmErr := c.Delete(wsMountPath(ws.ID), nil); rmErr != nil {
			cleanup = append(cleanup, "stop mount: "+rmErr.Error())
		}
		if rmErr := store.Delete(ws.ID); rmErr != nil {
			cleanup = append(cleanup, "delete metadata: "+rmErr.Error())
		}
		msg := fmt.Sprintf("mount %s at %s:%s: %v", ws.HostPath, ws.VM, ws.GuestPath, err)
		if st.Error != "" {
			msg += "\n" + st.Error
		}
		if len(cleanup) > 0 {
			msg += "\ncleanup failed (" + strings.Join(cleanup, "; ") + "); run `pvm workspace rm " + ws.ID + " -y`"
		}
		return fatalf("%s", msg)
	}
	fmt.Printf("mounted %s -> %s:%s over sftp", ws.HostPath, ws.VM, ws.GuestPath)
	if ws.ReadOnly {
		fmt.Print(" (read-only)")
	}
	fmt.Println()
	fmt.Println(watchNote)
	return 0
}

// uploadHomes lists the home directories the upload guard protects. Under
// the WSL2 backend DelegateToWSL passes in the Windows profile as well.
func uploadHomes() []string {
	var homes []string
	if h, err := os.UserHomeDir(); err == nil {
		homes = append(homes, h)
	}
	if h := os.Getenv(windowsHomeEnv); h != "" {
		homes = append(homes, h)
	}
	return homes
}

func wsMountPath(id string) string { return "/v1/workspaces/" + id + "/mount" }

func vmMountPath(ws *workspace.Workspace) string {
	return "/v1/vms/" + url.PathEscape(ws.VM) + "/mounts?guest=" + url.QueryEscape(ws.GuestPath)
}

// waitMounted starts the daemon's reverse-sshfs session and waits until the
// guest reports the mount. The daemon retries failed sessions itself, so an
// error state only fails the wait at the deadline.
func waitMounted(c *daemon.Client, id string, timeout time.Duration) (wsMountStatus, error) {
	var st wsMountStatus
	if err := c.Post(wsMountPath(id), nil, &st); err != nil {
		return st, err
	}
	deadline := time.Now().Add(timeout)
	for st.State != "mounted" {
		if st.State == "stopped" || st.State == "absent" {
			return st, fmt.Errorf("daemon reports the mount as %s", st.State)
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("not mounted after %s (state %s)", timeout, st.State)
		}
		time.Sleep(time.Second)
		if err := c.Get(wsMountPath(id), &st); err != nil {
			return st, err
		}
	}
	return st, nil
}

func createUpload(p *paths.Paths, store *workspace.Store, info *vmInfoView, ws *workspace.Workspace, yes bool) int {
	g, err := newWSGuest(p, info)
	if err != nil {
		return fatalf("%v", err)
	}
	m, err := workspace.LoadMatcher(ws.HostPath)
	if err != nil {
		return fatalf("%v", err)
	}
	if err := workspace.CheckUploadSource(ws.HostPath, uploadHomes(), p.Home, m); err != nil {
		return fatalf("%v", err)
	}
	host, skipped, err := workspace.ScanHost(ws.HostPath, m, nil)
	if err != nil {
		return fatalf("scan %s: %v", ws.HostPath, err)
	}
	printSkipped("not uploaded (not a file, directory or symlink)", skipped)
	files, size := workspace.Totals(host)
	fmt.Printf("uploading %d files (%s) to %s:%s\n", files, humanSize(size), ws.VM, ws.GuestPath)
	if size > largeUpload && !yes {
		if !stdinIsTTY() {
			return fatalf("refusing to upload %s without confirmation (non-interactive stdin); use -y to confirm", humanSize(size))
		}
		if !confirmAction(os.Stdin, os.Stderr, fmt.Sprintf("upload %s to %s?", humanSize(size), ws.VM)) {
			fmt.Fprintln(os.Stderr, "aborted")
			return 1
		}
	}
	out, err := g.output(guest.FreeSpaceCommand(), nil)
	if err != nil {
		return fatalf("check free space: %v", err)
	}
	free, err := guest.ParseFreeSpace(out)
	if err != nil {
		return fatalf("%v", err)
	}
	if need := size + 256<<20; need > free {
		gb := info.Spec.DiskGB + int((need-free+(1<<30)-1)>>30) + 1
		return fatalf("%s needs %s but %s has %s free; grow the disk with `pvm stop %s && pvm resize %s %d`",
			ws.HostPath, humanSize(size), ws.VM, humanSize(free), ws.VM, ws.VM, gb)
	}
	if err := prepareGuest(g, ws.GuestPath); err != nil {
		return fatalf("%v", err)
	}
	cmd, err := guest.UploadCommand(ws.GuestPath, true)
	if err != nil {
		return fatalf("%v", err)
	}
	if err := g.upload(cmd, func(w io.Writer) error {
		return workspace.WriteTar(w, ws.HostPath, host.Sorted(), "files/")
	}); err != nil {
		return fatalf("upload: %v\nnothing was saved; rerun `pvm workspace create`", err)
	}
	gscan, gskipped, err := g.scan(ws.GuestPath, m, nil)
	if err != nil {
		return fatalf("%v\nthe files were uploaded to %s:%s but the workspace was not saved; delete them with `pvm ssh %s rm -rf %s` and rerun", err, ws.VM, ws.GuestPath, ws.VM, ws.GuestPath)
	}
	printSkipped("skipped in the VM", gskipped)
	synced := map[string]bool{}
	for p := range host {
		synced[p] = true
	}
	for p := range gscan {
		synced[p] = true
	}
	bl := threeway.NextBaseline(nil, host, gscan, synced)
	var differ []string
	for p := range synced {
		if _, ok := bl.Content[p]; !ok {
			differ = append(differ, p)
		}
	}
	ws.LastTransfer = time.Now().UTC()
	if err := store.SaveBaseline(ws.ID, bl); err != nil {
		return fatalf("save baseline: %v", err)
	}
	if err := store.Save(ws); err != nil {
		return fatalf("save workspace %s: %v\nthe files are at %s:%s", ws.ID, err, ws.VM, ws.GuestPath)
	}
	fmt.Printf("created %s workspace %s at %s:%s\n", ws.Mode, ws.ID, ws.VM, ws.GuestPath)
	if len(differ) > 0 {
		sort.Strings(differ)
		fmt.Fprintf(os.Stderr, "warning: %d paths differ between the host and the VM after the upload:\n  %s\n", len(differ), strings.Join(differ, "\n  "))
	}
	if ws.Mode == workspace.ModeSync {
		fmt.Printf("next: pvm workspace ssh %s; pvm workspace pull %s brings VM changes back\n", ws.ID, ws.ID)
	} else {
		fmt.Printf("next: pvm workspace ssh %s; pvm workspace clone %s <dir> downloads the result\n", ws.ID, ws.ID)
	}
	return 0
}

func cmdWorkspaceList(args []string) int {
	if len(args) != 0 {
		return fatalf("usage: pvm workspace ls")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	list, err := workspace.NewStore(p).List()
	if err != nil {
		return fatalf("%v", err)
	}
	if len(list) == 0 {
		fmt.Println("no workspaces; create one with `pvm workspace create <id> <vm> --mode mount|sync|copy`")
		return 0
	}
	var c *daemon.Client
	healthy := false
	for _, ws := range list {
		if ws.Transport == workspace.TransportSFTP && c == nil {
			c = daemon.NewClient(p)
			healthy = c.Healthy()
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tVM\tMODE\tTRANSPORT\tHOST\tGUEST\tSTATE")
	for _, ws := range list {
		transport, state := ws.Transport, "-"
		if transport == "" {
			transport = "ssh"
		}
		if ws.ReadOnly {
			transport += " (ro)"
		}
		if ws.Transport == workspace.TransportSFTP {
			state = "unknown (daemon not running)"
			if healthy {
				var st wsMountStatus
				if err := c.Get(wsMountPath(ws.ID), &st); err != nil {
					state = "error: " + err.Error()
				} else if st.Error != "" {
					state = st.State + ": " + firstLine(st.Error)
				} else {
					state = st.State
				}
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ws.ID, ws.VM, ws.Mode, transport, ws.HostPath, ws.GuestPath, state)
	}
	tw.Flush()
	return 0
}

// wrongMode explains which command to use for a workspace that push, pull
// and status do not apply to.
func wrongMode(ws *workspace.Workspace, cmd string) int {
	if ws.Mode == workspace.ModeMount {
		return fatalf("workspace %s is a mount: the host and the VM share one copy, so there is nothing to %s", ws.ID, cmd)
	}
	return fatalf("workspace %s is in copy mode, %s applies to sync mode only; download the VM copy with `pvm workspace clone %s <dir>`", ws.ID, cmd, ws.ID)
}

// syncState is one three-way comparison of a sync workspace.
type syncState struct {
	ws          *workspace.Workspace
	g           *wsGuest
	m           *workspace.IgnoreMatcher
	base        *workspace.Baseline
	host, guest workspace.Manifest
}

func loadSync(p *paths.Paths, store *workspace.Store, id, cmd string) (*syncState, int) {
	ws, err := store.Load(id)
	if err != nil {
		return nil, fatalf("%v", err)
	}
	if ws.Mode != workspace.ModeSync {
		return nil, wrongMode(ws, cmd)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return nil, fatalf("%v", err)
	}
	info, err := getVMInfo(c, ws.VM)
	if err != nil {
		return nil, fatalf("%v", err)
	}
	g, err := newWSGuest(p, info)
	if err != nil {
		return nil, fatalf("%v", err)
	}
	base, err := store.LoadBaseline(id)
	if err != nil {
		return nil, fatalf("%v", err)
	}
	if base == nil {
		base = &workspace.Baseline{}
	}
	// The host's ignore rules decide both sides, so a path is either
	// synced everywhere or nowhere.
	m, err := workspace.LoadMatcher(ws.HostPath)
	if err != nil {
		return nil, fatalf("%v", err)
	}
	host, hskip, err := workspace.ScanHost(ws.HostPath, m, base.Host)
	if err != nil {
		return nil, fatalf("scan %s: %v", ws.HostPath, err)
	}
	gscan, gskip, err := g.scan(ws.GuestPath, m, base.Guest)
	if err != nil {
		return nil, fatalf("%v", err)
	}
	printSkipped("skipped on the host (not a file, directory or symlink)", hskip)
	printSkipped("skipped in the VM (not a file, directory or symlink)", gskip)
	return &syncState{ws: ws, g: g, m: m, base: base, host: host, guest: gscan}, 0
}

func cmdWorkspaceStatus(args []string) int {
	if len(args) != 1 {
		return fatalf("usage: pvm workspace status <id>")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	s, rc := loadSync(p, workspace.NewStore(p), args[0], "status")
	if s == nil {
		return rc
	}
	sum := threeway.Status(s.base, s.host, s.guest)
	writeStatus(os.Stdout, s.ws, sum)
	return 0
}

func writeStatus(w io.Writer, ws *workspace.Workspace, sum *threeway.Summary) {
	since := "creation"
	if !ws.LastTransfer.IsZero() {
		since = ws.LastTransfer.Local().Format(time.DateTime)
	}
	if len(sum.HostChanged)+len(sum.GuestChanged)+len(sum.Conflicts) == 0 {
		fmt.Fprintf(w, "%s: host and VM are in sync (last transfer %s)\n", ws.ID, since)
		return
	}
	for _, side := range []struct {
		name    string
		changes []threeway.Change
	}{{"host", sum.HostChanged}, {"VM", sum.GuestChanged}} {
		if len(side.changes) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s changes since %s:\n", side.name, since)
		for _, ch := range side.changes {
			fmt.Fprintf(w, "  %-9s %s\n", ch.Kind, ch.Path)
		}
	}
	if len(sum.Conflicts) > 0 {
		writeConflicts(w, ws, sum.Conflicts, "")
	} else {
		if len(sum.HostChanged) > 0 {
			fmt.Fprintf(w, "push with: pvm workspace push %s\n", ws.ID)
		}
		if len(sum.GuestChanged) > 0 {
			fmt.Fprintf(w, "pull with: pvm workspace pull %s\n", ws.ID)
		}
	}
}

func sideChange(base, e *workspace.Entry) string {
	switch {
	case e == nil && base == nil:
		return "absent"
	case e == nil:
		return "deleted"
	case base == nil:
		return "added"
	case e.SameContent(*base):
		if e.Type == workspace.TypeDir {
			return "changed inside"
		}
		return "unchanged"
	}
	return "modified"
}

func writeConflicts(w io.Writer, ws *workspace.Workspace, cs []threeway.Conflict, cmd string) {
	fmt.Fprintf(w, "conflicts (changed on both sides):\n")
	for _, c := range cs {
		fmt.Fprintf(w, "  %s (host: %s, VM: %s)\n", c.Path, sideChange(c.Base, c.Host), sideChange(c.Base, c.Guest))
	}
	if cmd == "" {
		fmt.Fprintf(w, "resolve with: pvm workspace push|pull %s --prefer host | --prefer vm | --keep-both\n", ws.ID)
		return
	}
	fmt.Fprintf(w, "nothing was written; resolve with one of:\n  pvm workspace %s %s --prefer host\n  pvm workspace %s %s --prefer vm\n  pvm workspace %s %s --keep-both\n",
		cmd, ws.ID, cmd, ws.ID, cmd, ws.ID)
}

func parseResolution(prefer string, keepBoth bool) (threeway.Resolution, error) {
	switch {
	case prefer != "" && keepBoth:
		return threeway.None, fmt.Errorf("--prefer and --keep-both are mutually exclusive")
	case keepBoth:
		return threeway.KeepBoth, nil
	case prefer == "host":
		return threeway.PreferHost, nil
	case prefer == "vm":
		return threeway.PreferVM, nil
	case prefer == "":
		return threeway.None, nil
	}
	return threeway.None, fmt.Errorf("--prefer must be host or vm")
}

func cmdWorkspaceTransfer(args []string, dir threeway.Direction) int {
	name := "push"
	if dir == threeway.Pull {
		name = "pull"
	}
	fs := newFlagSet("workspace "+name, "<id> [--prefer host|vm | --keep-both]")
	prefer := fs.String("prefer", "", "resolve conflicts with this side's version: host or vm")
	keepBoth := fs.Bool("keep-both", false, "resolve conflicts by keeping the other side's version as <name>.pvm-conflict-<host|vm>")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	res, err := parseResolution(*prefer, *keepBoth)
	if err != nil {
		return fatalf("%v", err)
	}
	id := pos[0]
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	store := workspace.NewStore(p)
	lock, err := store.Lock(id)
	if err != nil {
		return fatalf("%v", err)
	}
	defer lock.Unlock()
	s, rc := loadSync(p, store, id, name)
	if s == nil {
		return rc
	}
	pl := threeway.NewPlan(s.base, s.host, s.guest, dir, res)
	if pl.Unresolved {
		writeConflicts(os.Stderr, s.ws, pl.Conflicts, name)
		return 1
	}
	// A pull uploads too when it resolves conflicts with the host's side.
	if len(pl.ToGuest) > 0 {
		if err := workspace.CheckUploadSource(s.ws.HostPath, uploadHomes(), p.Home, s.m); err != nil {
			return fatalf("%v", err)
		}
	}
	retry := fmt.Sprintf("pvm workspace %s %s", name, strings.Join(args, " "))
	written, rejected, err := s.apply(pl)
	if err != nil && len(written) > 0 {
		fmt.Fprintf(os.Stderr, "written to the host before the failure:\n  %s\n", strings.Join(written, "\n  "))
	}
	if paths := workspace.HostExecutable(written); len(paths) > 0 {
		fmt.Fprintf(os.Stderr, "review before running host tools; these files can run code on the host:\n  %s\n", strings.Join(paths, "\n  "))
	}
	if err != nil {
		printSkipped("not written to the host", rejected)
		return fatalf("%s: %v\nthe baseline was not updated; rerun `%s`", name, err, retry)
	}
	if len(rejected) > 0 {
		printSkipped("not written to the host", rejected)
		return fatalf("%d paths were not written and the baseline was not updated; fix them in the VM and rerun `%s`", len(rejected), retry)
	}
	host, _, err := workspace.ScanHost(s.ws.HostPath, s.m, s.hostPrev(pl))
	if err != nil {
		return fatalf("rescan %s: %v\nthe baseline was not updated; rerun `%s`", s.ws.HostPath, err, retry)
	}
	gscan, _, err := s.g.scan(s.ws.GuestPath, s.m, s.guest)
	if err != nil {
		return fatalf("rescan VM: %v\nthe baseline was not updated; rerun `%s`", err, retry)
	}
	s.ws.LastTransfer = time.Now().UTC()
	if err := store.SaveBaseline(id, threeway.NextBaseline(s.base, host, gscan, pl.Synced)); err != nil {
		return fatalf("save baseline: %v", err)
	}
	if err := store.Save(s.ws); err != nil {
		return fatalf("save workspace %s: %v", id, err)
	}
	writeTransferSummary(os.Stdout, s.ws, name, pl)
	return 0
}

// hostPrev seeds the post-transfer host scan with the entries just
// written, so Windows hosts keep the exec bit of files from the VM.
func (s *syncState) hostPrev(pl *threeway.Plan) workspace.Manifest {
	prev := workspace.Manifest{}
	for p, e := range s.host {
		prev[p] = e
	}
	for _, r := range pl.RenameOnHost {
		for p, e := range s.host {
			if p == r.From || strings.HasPrefix(p, r.From+"/") {
				e.Path = r.To + p[len(r.From):]
				prev[e.Path] = e
			}
		}
	}
	for _, e := range pl.ToHost {
		prev[e.Path] = e
	}
	return prev
}

func writeTransferSummary(w io.Writer, ws *workspace.Workspace, name string, pl *threeway.Plan) {
	if len(pl.ToGuest)+len(pl.DeleteOnGuest)+len(pl.ToHost)+len(pl.DeleteOnHost) == 0 {
		fmt.Fprintf(w, "%s: nothing to %s\n", ws.ID, name)
	} else {
		fmt.Fprintf(w, "%s: VM %d copied, %d deleted; host %d copied, %d deleted\n",
			ws.ID, len(pl.ToGuest), len(pl.DeleteOnGuest), len(pl.ToHost), len(pl.DeleteOnHost))
	}
	for _, r := range pl.RenameOnGuest {
		fmt.Fprintf(w, "kept both: the VM version of %s is now %s on both sides\n", r.From, r.To)
	}
	for _, r := range pl.RenameOnHost {
		fmt.Fprintf(w, "kept both: the host version of %s is now %s on both sides\n", r.From, r.To)
	}
	if len(pl.Pending) > 0 {
		other, side := "pull", "VM"
		if name == "pull" {
			other, side = "push", "host"
		}
		fmt.Fprintf(w, "%d paths changed only on the %s and were not copied; run `pvm workspace %s %s`\n", len(pl.Pending), side, other, ws.ID)
	}
}

// apply carries out a plan: renames on both sides first, then the VM's
// deletes and copies in one upload, then the host's. It returns the host
// paths written and the downloaded entries that were rejected.
func (s *syncState) apply(pl *threeway.Plan) ([]string, []workspace.Skipped, error) {
	ws := s.ws
	root, err := os.OpenRoot(ws.HostPath)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	for _, r := range pl.RenameOnHost {
		if err := renameInRoot(root, r.From, r.To); err != nil {
			return nil, nil, fmt.Errorf("host: %w", err)
		}
	}
	if len(pl.RenameOnGuest) > 0 {
		cmd, err := guest.RenameCommand(ws.GuestPath)
		if err != nil {
			return nil, nil, err
		}
		var pairs []string
		for _, r := range pl.RenameOnGuest {
			pairs = append(pairs, r.From, r.To)
		}
		if _, err := s.g.output(cmd, nulList(pairs)); err != nil {
			return nil, nil, fmt.Errorf("rename in VM: %w", err)
		}
	}
	if len(pl.ToGuest)+len(pl.DeleteOnGuest) > 0 {
		cmd, err := guest.UploadCommand(ws.GuestPath, false)
		if err != nil {
			return nil, nil, err
		}
		if err := s.g.upload(cmd, func(w io.Writer) error {
			return writeUpload(w, ws.HostPath, pl.ToGuest, pl.DeleteOnGuest)
		}); err != nil {
			return nil, nil, fmt.Errorf("upload: %w", err)
		}
	}
	for _, rel := range pl.DeleteOnHost {
		if err := workspace.RemoveFromRoot(root, rel); err != nil {
			return nil, nil, fmt.Errorf("host: delete %s: %w", rel, err)
		}
	}
	if len(pl.ToHost) == 0 {
		return nil, nil, nil
	}
	opts := workspace.DefaultExtractOptions()
	opts.Overwrite = true
	opts.Only = map[string]bool{}
	opts.Expect = workspace.Manifest{}
	var names []string
	for _, e := range pl.ToHost {
		opts.Only[e.Path] = true
		opts.Expect[e.Path] = e
		names = append(names, e.Path)
	}
	cmd, err := guest.DownloadCommand(ws.GuestPath)
	if err != nil {
		return nil, nil, err
	}
	var res *workspace.ExtractResult
	err = s.g.download(cmd, nulList(names), func(r io.Reader) error {
		var err error
		res, err = workspace.ExtractTar(r, root, opts)
		return err
	})
	if res == nil {
		res = &workspace.ExtractResult{}
	}
	rejected := res.Rejected
	for _, l := range res.SkippedSymlinks {
		rejected = append(rejected, workspace.Skipped{Path: l, Reason: "this host cannot create symlinks"})
	}
	got := map[string]bool{}
	for _, w := range res.Written {
		got[w] = true
	}
	for _, r := range rejected {
		got[r.Path] = true
	}
	if err == nil {
		for _, n := range names {
			if !got[n] {
				rejected = append(rejected, workspace.Skipped{Path: n, Reason: "missing from the VM's archive"})
			}
		}
	}
	if err != nil {
		return res.Written, rejected, fmt.Errorf("download: %w", err)
	}
	return res.Written, rejected, nil
}

func renameInRoot(root *os.Root, from, to string) error {
	if _, err := root.Lstat(filepath.FromSlash(to)); err == nil {
		return fmt.Errorf("cannot rename %s: %s exists", from, to)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.Rename(filepath.FromSlash(from), filepath.FromSlash(to))
}

// writeUpload builds the stream guest.UploadCommand reads: an optional
// pvm/deletes list, then the new contents under files/.
func writeUpload(w io.Writer, root string, entries []workspace.Entry, deletes []string) error {
	if len(deletes) > 0 {
		data := nulList(deletes)
		tw := tar.NewWriter(w)
		hdr := &tar.Header{Name: "pvm/deletes", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)), ModTime: time.Now(), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
		// Flush, not Close: WriteTar continues the same archive.
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return workspace.WriteTar(w, root, entries, "files/")
}

// checkCloneDest returns the absolute destination, which must be missing
// or an empty directory.
func checkCloneDest(dest string) (string, error) {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	ents, err := os.ReadDir(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, lerr := os.Lstat(abs); lerr == nil {
			return "", fmt.Errorf("%s exists and is not a directory", abs)
		}
		return abs, nil
	case err != nil:
		return "", err
	case len(ents) > 0:
		return "", fmt.Errorf("%s is not empty; clone never overwrites files", abs)
	}
	return abs, nil
}

func cmdWorkspaceClone(args []string) int {
	if len(args) != 2 {
		return fatalf("usage: pvm workspace clone <id> <dest-dir>")
	}
	id := args[0]
	dest, err := checkCloneDest(args[1])
	if err != nil {
		return fatalf("%v", err)
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	store := workspace.NewStore(p)
	lock, err := store.Lock(id)
	if err != nil {
		return fatalf("%v", err)
	}
	defer lock.Unlock()
	ws, err := store.Load(id)
	if err != nil {
		return fatalf("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fatalf("%v", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".pvm-clone-*")
	if err != nil {
		return fatalf("%v", err)
	}
	keepTmp := false
	defer func() {
		if !keepTmp {
			os.RemoveAll(tmp)
		}
	}()
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return fatalf("%v", err)
	}
	defer root.Close()

	var res *workspace.ExtractResult
	var gscan workspace.Manifest
	var base *workspace.Baseline
	if ws.Mode == workspace.ModeMount {
		res, err = cloneLocal(ws.HostPath, root)
	} else {
		res, gscan, base, err = cloneGuest(p, store, ws, root)
	}
	if res == nil {
		res = &workspace.ExtractResult{}
	}
	if err != nil {
		printSkipped("rejected", res.Rejected)
		return fatalf("clone %s: %v\nnothing was written to %s; rerun `pvm workspace clone %s %s`", id, err, dest, id, dest)
	}
	root.Close()
	// Windows cannot rename over an existing directory, even an empty one.
	if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fatalf("%v", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		keepTmp = true
		return fatalf("move clone into place: %v; the files are in %s", err, tmp)
	}
	fmt.Printf("cloned %s into %s (%d paths)\n", id, dest, len(res.Written))
	if paths := workspace.HostExecutable(res.Written); len(paths) > 0 {
		fmt.Fprintf(os.Stderr, "review before running host tools; these files can run code on the host:\n  %s\n", strings.Join(paths, "\n  "))
	}
	if len(res.SkippedSymlinks) > 0 {
		fmt.Fprintf(os.Stderr, "symlinks skipped (this host cannot create symlinks):\n  %s\n", strings.Join(res.SkippedSymlinks, "\n  "))
	}
	if len(res.Rejected) > 0 {
		printSkipped("not cloned", res.Rejected)
		return fatalf("%d paths were not cloned; see the list above", len(res.Rejected))
	}
	if ws.Mode == workspace.ModeCopy {
		// The VM copy is now saved on the host; `rm` warns only about
		// changes made after this point.
		base.Content, base.Guest = gscan, gscan
		ws.LastTransfer = time.Now().UTC()
		if err := store.SaveBaseline(id, base); err != nil {
			return fatalf("save baseline: %v", err)
		}
		if err := store.Save(ws); err != nil {
			return fatalf("save workspace %s: %v", id, err)
		}
	}
	return 0
}

// cloneLocal copies a mount workspace's host folder, skipping ignored
// paths and copying symlinks as links.
func cloneLocal(src string, dest *os.Root) (*workspace.ExtractResult, error) {
	m, err := workspace.LoadMatcher(src)
	if err != nil {
		return nil, err
	}
	man, skipped, err := workspace.ScanHost(src, m, nil)
	if err != nil {
		return nil, err
	}
	printSkipped("not cloned (not a file, directory or symlink)", skipped)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(workspace.WriteTar(pw, src, man.Sorted(), "")) }()
	opts := workspace.DefaultExtractOptions()
	opts.Expect = man
	res, err := workspace.ExtractTar(pr, dest, opts)
	pr.CloseWithError(errors.New("extraction stopped"))
	return res, err
}

func cloneGuest(p *paths.Paths, store *workspace.Store, ws *workspace.Workspace, dest *os.Root) (*workspace.ExtractResult, workspace.Manifest, *workspace.Baseline, error) {
	c, err := daemon.Ensure(p)
	if err != nil {
		return nil, nil, nil, err
	}
	info, err := getVMInfo(c, ws.VM)
	if err != nil {
		return nil, nil, nil, err
	}
	g, err := newWSGuest(p, info)
	if err != nil {
		return nil, nil, nil, err
	}
	base, err := store.LoadBaseline(ws.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	if base == nil {
		base = &workspace.Baseline{}
	}
	all, skipped, err := g.list(ws.GuestPath)
	if err != nil {
		return nil, nil, nil, err
	}
	m, err := g.matcher(ws.GuestPath, all)
	if err != nil {
		return nil, nil, nil, err
	}
	man, skipped, err := g.complete(ws.GuestPath, all, skipped, m, base.Guest)
	if err != nil {
		return nil, nil, nil, err
	}
	printSkipped("not cloned (not a file, directory or symlink)", skipped)
	files, size := workspace.Totals(man)
	fmt.Printf("downloading %d files (%s) from %s:%s\n", files, humanSize(size), ws.VM, ws.GuestPath)
	sorted := man.Sorted()
	names := make([]string, len(sorted))
	opts := workspace.DefaultExtractOptions()
	opts.Only = map[string]bool{}
	opts.Expect = man
	for i, e := range sorted {
		names[i] = e.Path
		opts.Only[e.Path] = true
	}
	cmd, err := guest.DownloadCommand(ws.GuestPath)
	if err != nil {
		return nil, nil, nil, err
	}
	var res *workspace.ExtractResult
	err = g.download(cmd, nulList(names), func(r io.Reader) error {
		var err error
		res, err = workspace.ExtractTar(r, dest, opts)
		return err
	})
	return res, man, base, err
}

func cmdWorkspaceSSH(args []string) int {
	if len(args) < 1 {
		return fatalf("usage: pvm workspace ssh <id> [command...]")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	ws, err := workspace.NewStore(p).Load(args[0])
	if err != nil {
		return fatalf("%v", err)
	}
	return CmdSSH(workspaceSSHArgs(ws, args[1:]))
}

// workspaceSSHArgs turns `workspace ssh` into `pvm ssh` arguments that
// start in the workspace directory.
func workspaceSSHArgs(ws *workspace.Workspace, command []string) []string {
	cd := "cd " + sshShellQuote(ws.GuestPath) + " && "
	if len(command) == 0 {
		return []string{"-t", ws.VM, cd + `exec "${SHELL:-/bin/sh}" -l`}
	}
	return []string{ws.VM, cd + strings.Join(command, " ")}
}

// workspaceUnsaved lists the VM-side changes of a sync or copy workspace
// that have not been pulled or cloned to the host.
func workspaceUnsaved(g *wsGuest, store *workspace.Store, ws *workspace.Workspace) ([]threeway.Change, error) {
	base, err := store.LoadBaseline(ws.ID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		base = &workspace.Baseline{}
	}
	all, skipped, err := g.list(ws.GuestPath)
	if err != nil {
		return nil, err
	}
	var m *workspace.IgnoreMatcher
	if ws.Mode == workspace.ModeSync {
		m, err = workspace.LoadMatcher(ws.HostPath)
	} else {
		m, err = g.matcher(ws.GuestPath, all)
	}
	if err != nil {
		return nil, err
	}
	man, _, err := g.complete(ws.GuestPath, all, skipped, m, base.Guest)
	if err != nil {
		return nil, err
	}
	return threeway.Diff(base, man), nil
}

func unsavedNote(ws *workspace.Workspace, changes []threeway.Change) string {
	how := "pvm workspace clone " + ws.ID + " <dir>"
	if ws.Mode == workspace.ModeSync {
		how = "pvm workspace pull " + ws.ID
	}
	return fmt.Sprintf("the VM copy has %d changes that were not saved to the host (%s)", len(changes), how)
}

func cmdWorkspaceRm(args []string) int {
	fs := newFlagSet("workspace rm", "<id> [--delete-guest] [-y]")
	deleteGuest := fs.Bool("delete-guest", false, "also delete the VM copy at /workspace/<id>")
	yes := fs.Bool("y", false, "confirm (non-interactive)")
	if err := fs.Parse(partitionFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	store := workspace.NewStore(p)
	lock, err := store.Lock(id)
	if err != nil {
		return fatalf("%v", err)
	}
	defer lock.Unlock()
	ws, err := store.Load(id)
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	info, vmErr := getVMInfo(c, ws.VM)
	var g *wsGuest
	if vmErr == nil && info.Running {
		g, vmErr = newWSGuest(p, info)
	}
	if *deleteGuest && g == nil {
		if vmErr == nil {
			vmErr = requireRunning(info)
		}
		return fatalf("--delete-guest needs the VM: %v", vmErr)
	}

	var warnings []string
	if ws.Mode != workspace.ModeMount && g != nil {
		if changes, err := workspaceUnsaved(g, store, ws); err != nil {
			warnings = append(warnings, "could not check the VM copy for unsaved changes: "+err.Error())
		} else if len(changes) > 0 {
			warnings = append(warnings, unsavedNote(ws, changes))
		}
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: workspace %s: %s\n", id, w)
	}
	guestFate := "kept"
	if *deleteGuest {
		guestFate = "deleted"
	}
	if !*yes {
		if !stdinIsTTY() {
			return fatalf("refusing to remove workspace %q without confirmation (non-interactive stdin); use -y to confirm", id)
		}
		prompt := fmt.Sprintf("remove workspace '%s'? host files in %s are kept; the VM copy at %s:%s is %s", id, ws.HostPath, ws.VM, ws.GuestPath, guestFate)
		if !confirmAction(os.Stdin, os.Stderr, prompt) {
			fmt.Fprintln(os.Stderr, "aborted")
			return 1
		}
	}

	if ws.Mode == workspace.ModeMount {
		if err := removeWorkspaceMount(c, ws, info, vmErr); err != nil {
			return fatalf("%v\nworkspace %s was kept; rerun `pvm workspace rm %s` when fixed", err, id, id)
		}
	}
	if *deleteGuest {
		if err := deleteGuestCopy(g, ws); err != nil {
			return fatalf("%v\nworkspace %s was kept; rerun `pvm workspace rm %s --delete-guest`", err, id, id)
		}
	}
	if err := store.Delete(id); err != nil {
		return fatalf("%v", err)
	}
	fmt.Printf("removed workspace %s; host files in %s were not touched", id, ws.HostPath)
	if *deleteGuest {
		fmt.Printf("; deleted %s:%s\n", ws.VM, ws.GuestPath)
	} else if vmErr == nil {
		fmt.Printf("; the VM copy (if any) stays at %s:%s\n", ws.VM, ws.GuestPath)
	} else {
		fmt.Println()
	}
	return 0
}

// removeWorkspaceMount detaches a mount workspace before its metadata is
// deleted; the daemon needs the metadata to find the sftp mount.
func removeWorkspaceMount(c *daemon.Client, ws *workspace.Workspace, info *vmInfoView, vmErr error) error {
	if ws.Transport == workspace.TransportSFTP {
		if err := c.Delete(wsMountPath(ws.ID), nil); err != nil {
			return fmt.Errorf("unmount: %w", err)
		}
		return nil
	}
	if info == nil {
		fmt.Fprintf(os.Stderr, "note: vm %s is unavailable (%v); no mount to remove\n", ws.VM, vmErr)
		return nil
	}
	var mounts []mountView
	if err := c.Get("/v1/vms/"+url.PathEscape(ws.VM)+"/mounts", &mounts); err != nil {
		return fmt.Errorf("list mounts of %s: %w", ws.VM, err)
	}
	for _, m := range mounts {
		if m.Guest == ws.GuestPath {
			if err := c.Delete(vmMountPath(ws), nil); err != nil {
				return fmt.Errorf("unmount %s:%s: %w", ws.VM, ws.GuestPath, err)
			}
			return nil
		}
	}
	return nil
}

func deleteGuestCopy(g *wsGuest, ws *workspace.Workspace) error {
	if ws.Mode == workspace.ModeMount {
		// rm -rf through a live mount would delete host files.
		cmd, err := guest.MountedCommand(ws.GuestPath)
		if err != nil {
			return err
		}
		out, err := g.output(cmd, nil)
		if err != nil {
			return fmt.Errorf("check mount: %w", err)
		}
		if mounted, err := guest.ParseMounted(out); err != nil {
			return err
		} else if mounted {
			return fmt.Errorf("%s:%s is still mounted; not deleting it", ws.VM, ws.GuestPath)
		}
	}
	cmd, err := guest.RemoveCommand(ws.GuestPath)
	if err != nil {
		return err
	}
	if _, err := g.output(cmd, nil); err != nil {
		return fmt.Errorf("delete %s:%s: %w", ws.VM, ws.GuestPath, err)
	}
	return nil
}

func vmRmWorkspaceNotes(p *paths.Paths, c *daemon.Client, name string) []string {
	var g *wsGuest
	var gErr error
	return vmWorkspaceNotes(p, name, func(ws *workspace.Workspace) ([]threeway.Change, error) {
		if g == nil && gErr == nil {
			var info *vmInfoView
			if info, gErr = getVMInfo(c, name); gErr == nil {
				g, gErr = newWSGuest(p, info)
			}
		}
		if gErr != nil {
			return nil, gErr
		}
		return workspaceUnsaved(g, workspace.NewStore(p), ws)
	})
}

// vmWorkspaceNotes describes the workspaces bound to a VM for `pvm rm`.
// Errors are reported in the notes, never returned: they must not block
// deleting the VM.
func vmWorkspaceNotes(p *paths.Paths, name string, unsaved func(*workspace.Workspace) ([]threeway.Change, error)) []string {
	store := workspace.NewStore(p)
	list, err := store.List()
	if err != nil {
		return []string{"could not list workspaces: " + err.Error()}
	}
	var notes []string
	for _, ws := range list {
		if ws.VM != name {
			continue
		}
		line := fmt.Sprintf("workspace %s (%s): %s -> %s", ws.ID, ws.Mode, ws.HostPath, ws.GuestPath)
		if ws.Mode != workspace.ModeMount && unsaved != nil {
			if changes, err := unsaved(ws); err != nil {
				line += "; could not check the VM copy: " + err.Error()
			} else if len(changes) > 0 {
				line += "; " + unsavedNote(ws, changes)
			}
		}
		notes = append(notes, line)
	}
	if len(notes) > 0 {
		notes = append(notes, "these workspaces are kept as orphans; remove them with `pvm workspace rm <id>`")
	}
	return notes
}
