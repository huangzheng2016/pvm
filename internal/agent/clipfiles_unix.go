//go:build !windows

package agent

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// maxClipFiles bounds the files of one clipboard copy, either way.
const maxClipFiles = 256 << 20

// clipFilesPy owns the clipboard with a list of files; see the file.
//
//go:embed clip_files.py
var clipFilesPy string

// clipFilesScript runs as the VM user with a tar of the host's copied
// files on stdin. It unpacks them under the user's cache, replacing the
// previous copy, and puts them on the clipboard.
const clipFilesScript = `set -e
base="$HOME/.cache/pvm/clipboard"
mkdir -p "$base"
find "$base" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
d=$(mktemp -d "$base/XXXXXX")
tar -x -C "$d" --no-same-owner
set --
for f in "$d"/* "$d"/.[!.]* "$d"/..?*; do
  [ -e "$f" ] && set -- "$@" "$f"
done
[ $# -gt 0 ] || { echo "no files arrived" >&2; exit 1; }
exec python3 -c "$PVM_CLIP_FILES_PY" "$@"
`

// clipFilesSource sends the files on uid's clipboard to the host. It
// first writes a line with a fingerprint of the files, empty when the
// clipboard holds none, then waits for one byte from the host: 'y' asks
// for the files as a tar, anything else ends the stream, so the host
// skips files it already holds. Files that came from the host are not
// offered back.
func clipFilesSource(uid int) (io.ReadWriteCloser, error) {
	u, err := clipUser(uid)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := asUser(ctx, u, nil, "sh", "-c", "xclip -selection clipboard -o -t x-special/gnome-copied-files 2>/dev/null || xclip -selection clipboard -o -t text/uri-list 2>/dev/null || true").Output()
	if err != nil {
		return nil, fmt.Errorf("read clipboard: %w", err)
	}
	paths := clipFilePaths(string(out), filepath.Join(u.HomeDir, ".cache", "pvm", "clipboard"))
	key := ""
	if len(paths) > 0 {
		h := sha256.New()
		for _, p := range paths {
			fmt.Fprintf(h, "%s\x00", p)
			if fi, err := os.Lstat(p); err == nil {
				fmt.Fprintf(h, "%d %d\x00", fi.Size(), fi.ModTime().UnixNano())
			}
		}
		key = hex.EncodeToString(h.Sum(nil))
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		defer inR.Close()
		if _, err := io.WriteString(outW, key+"\n"); err != nil || key == "" {
			outW.Close()
			return
		}
		var b [1]byte
		if _, err := io.ReadFull(inR, b[:]); err != nil || b[0] != 'y' {
			outW.Close()
			return
		}
		go io.Copy(io.Discard, inR)
		args := []string{"tar", "-c", "-f", "-"}
		for _, p := range paths {
			args = append(args, "-C", path.Dir(p), "./"+path.Base(p))
		}
		cmd := asUser(context.Background(), u, nil, args...)
		cmd.Stdout = outW
		outW.CloseWithError(cmd.Run())
	}()
	return &echoConn{r: outR, w: inW}, nil
}

// clipFilePaths returns the local files of a gnome-copied-files or
// uri-list selection, leaving out those under skip.
func clipFilePaths(sel, skip string) []string {
	var paths []string
	for _, line := range strings.Split(sel, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "file://") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			continue
		}
		p := path.Clean(u.Path)
		if !path.IsAbs(p) || p == "/" || p == skip || strings.HasPrefix(p, skip+"/") {
			continue
		}
		paths = append(paths, p)
	}
	return paths
}
