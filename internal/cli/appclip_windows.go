package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const clipboardBridgeSupported = true

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procOpenClipboard       = user32.NewProc("OpenClipboard")
	procCloseClipboard      = user32.NewProc("CloseClipboard")
	procGetClipboardData    = user32.NewProc("GetClipboardData")
	procIsFormatAvailable   = user32.NewProc("IsClipboardFormatAvailable")
	procRegisterFormat      = user32.NewProc("RegisterClipboardFormatW")
	procGetClipboardSeq     = user32.NewProc("GetClipboardSequenceNumber")
	procGetClipboardOwner   = user32.NewProc("GetClipboardOwner")
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procGetWindowThreadPID  = user32.NewProc("GetWindowThreadProcessId")
	procGlobalLock          = kernel32.NewProc("GlobalLock")
	procGlobalUnlock        = kernel32.NewProc("GlobalUnlock")
	procGlobalSize          = kernel32.NewProc("GlobalSize")
	procMoveMemory          = kernel32.NewProc("RtlMoveMemory")
)

const (
	cfUnicodeText = 13
	cfDIB         = 8
	maxClipText   = 1 << 20
	maxClipImage  = 16 << 20
)

var (
	cfPNG = registerFormat("PNG")
	// password managers mark secrets with this format so that clipboard
	// tools leave them alone
	cfExclude = registerFormat("ExcludeClipboardContentFromMonitorProcessing")
)

func registerFormat(name string) uintptr {
	p, _ := syscall.UTF16PtrFromString(name)
	r, _, _ := procRegisterFormat.Call(uintptr(unsafe.Pointer(p)))
	return r
}

func clipboardSeq() uint32 {
	r, _, _ := procGetClipboardSeq.Call()
	return uint32(r)
}

// windowProcess returns the image path of the process owning hwnd.
func windowProcess(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	var pid uint32
	procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

func windowTitle(hwnd uintptr) string {
	buf := make([]uint16, 512)
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

// inDir reports whether path is a file under dir, case-insensitively.
func inDir(path, dir string) bool {
	rel, err := filepath.Rel(strings.ToLower(dir), strings.ToLower(path))
	return err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// clipContent is one snapshot of the host clipboard.
type clipContent struct {
	kind string // "text" or "png"
	data []byte
}

// readClipboard returns the clipboard as text or PNG; ok is false when it
// holds neither, holds a secret, or cannot be opened.
func readClipboard() (clipContent, bool) {
	var opened bool
	for i := 0; i < 10; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !opened {
		return clipContent{}, false
	}
	defer procCloseClipboard.Call()
	if cfExclude != 0 && formatAvailable(cfExclude) {
		return clipContent{}, false
	}
	if cfPNG != 0 && formatAvailable(cfPNG) {
		if b := clipboardBytes(cfPNG, maxClipImage); b != nil {
			return clipContent{"png", b}, true
		}
	}
	if formatAvailable(cfUnicodeText) {
		if b := clipboardBytes(cfUnicodeText, 2*maxClipText); b != nil {
			u := unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), len(b)/2)
			if i := indexZero(u); i >= 0 {
				u = u[:i]
			}
			return clipContent{"text", []byte(string(utf16.Decode(u)))}, true
		}
	}
	if formatAvailable(cfDIB) {
		if b := clipboardBytes(cfDIB, maxClipImage); b != nil {
			if p, err := dibToPNG(b); err == nil {
				return clipContent{"png", p}, true
			}
		}
	}
	return clipContent{}, false
}

func indexZero(u []uint16) int {
	for i, c := range u {
		if c == 0 {
			return i
		}
	}
	return -1
}

func formatAvailable(f uintptr) bool {
	r, _, _ := procIsFormatAvailable.Call(f)
	return r != 0
}

// clipboardBytes copies one clipboard format, or nil when it is larger
// than limit or unreadable.
func clipboardBytes(format uintptr, limit int) []byte {
	h, _, _ := procGetClipboardData.Call(format)
	if h == 0 {
		return nil
	}
	size, _, _ := procGlobalSize.Call(h)
	if size == 0 || int(size) > limit {
		return nil
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return nil
	}
	defer procGlobalUnlock.Call(h)
	buf := make([]byte, size)
	procMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), p, size)
	return buf
}

// dibToPNG converts an uncompressed 24 or 32 bit CF_DIB to PNG.
func dibToPNG(b []byte) ([]byte, error) {
	if len(b) < 40 {
		return nil, fmt.Errorf("short DIB header")
	}
	hdr := binary.LittleEndian.Uint32(b[0:4])
	w := int(int32(binary.LittleEndian.Uint32(b[4:8])))
	h := int(int32(binary.LittleEndian.Uint32(b[8:12])))
	bpp := int(binary.LittleEndian.Uint16(b[14:16]))
	comp := binary.LittleEndian.Uint32(b[16:20])
	const biRGB, biBitfields = 0, 3
	if (bpp != 24 && bpp != 32) || (comp != biRGB && comp != biBitfields) || w <= 0 || h == 0 || w > 1<<15 {
		return nil, fmt.Errorf("unsupported DIB: %d bpp, compression %d", bpp, comp)
	}
	off := int(hdr)
	if comp == biBitfields && hdr == 40 {
		off += 12 // the three color masks follow a plain BITMAPINFOHEADER
	}
	bottomUp := h > 0
	if !bottomUp {
		h = -h
	}
	stride := (w*bpp/8 + 3) &^ 3
	if off+stride*h > len(b) {
		return nil, fmt.Errorf("truncated DIB")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	alpha := false
	for y := 0; y < h && bpp == 32; y++ {
		row := b[off+y*stride:]
		for x := 0; x < w; x++ {
			if row[x*4+3] != 0 {
				alpha = true
				break
			}
		}
	}
	for y := 0; y < h; y++ {
		src := y
		if bottomUp {
			src = h - 1 - y
		}
		row := b[off+src*stride:]
		for x := 0; x < w; x++ {
			px := row[x*bpp/8:]
			a := uint8(255)
			if bpp == 32 && alpha {
				a = px[3]
			}
			img.SetNRGBA(x, y, color.NRGBA{R: px[2], G: px[1], B: px[0], A: a})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// runClipboardBridge copies the host clipboard into vm while one of its
// Xpra windows is in the foreground: when such a window gains focus, and
// when the clipboard changes while it has it. Content the Xpra client
// itself put on the clipboard came from the guest and is not sent back.
// It returns when the client process exits.
func runClipboardBridge(vm string, clientPID int, clientDir string, send func(clipContent) error) error {
	proc, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(clientPID))
	if err != nil {
		return fmt.Errorf("watch xpra client: %w", err)
	}
	defer windows.CloseHandle(proc)
	suffix := " [" + vm + "]"
	var focused bool
	var seenSeq uint32
	var sent [32]byte
	for {
		if ev, _ := windows.WaitForSingleObject(proc, 150); ev == windows.WAIT_OBJECT_0 {
			return nil
		}
		fg, _, _ := procGetForegroundWindow.Call()
		nowFocused := fg != 0 && strings.HasSuffix(windowTitle(fg), suffix) && inDir(windowProcess(fg), clientDir)
		seq := clipboardSeq()
		if nowFocused && (!focused || seq != seenSeq) {
			owner, _, _ := procGetClipboardOwner.Call()
			if !inDir(windowProcess(owner), clientDir) {
				if c, ok := readClipboard(); ok {
					if sum := sha256.Sum256(append([]byte(c.kind), c.data...)); sum != sent {
						if err := send(c); err != nil {
							fmt.Fprintf(os.Stderr, "clipboard to %s: %v\n", vm, err)
						} else {
							sent = sum
						}
					}
				}
			}
		}
		focused, seenSeq = nowFocused, seq
	}
}

// sendClipboard writes one snapshot to the guest and waits until the
// guest holds it.
func sendClipboard(open func(kind string) (io.ReadWriteCloser, error), c clipContent) error {
	s, err := open(c.kind)
	if err != nil {
		return err
	}
	defer s.Close()
	if _, err := s.Write(c.data); err != nil {
		return err
	}
	if cw, ok := s.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	_, err = io.Copy(io.Discard, s)
	return err
}
