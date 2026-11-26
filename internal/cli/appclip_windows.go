package cli

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
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
	procEmptyClipboard      = user32.NewProc("EmptyClipboard")
	procSetClipboardData    = user32.NewProc("SetClipboardData")
	procCreateWindowEx      = user32.NewProc("CreateWindowExW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procMsgWaitForMultiple  = user32.NewProc("MsgWaitForMultipleObjects")
	procPeekMessage         = user32.NewProc("PeekMessageW")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procGlobalAlloc         = kernel32.NewProc("GlobalAlloc")
	procGlobalFree          = kernel32.NewProc("GlobalFree")
	procGlobalLock          = kernel32.NewProc("GlobalLock")
	procGlobalUnlock        = kernel32.NewProc("GlobalUnlock")
	procGlobalSize          = kernel32.NewProc("GlobalSize")
	procMoveMemory          = kernel32.NewProc("RtlMoveMemory")
)

const (
	cfUnicodeText = 13
	cfDIB         = 8
	cfHDrop       = 15
	hwndMessage   = ^uintptr(2) // HWND_MESSAGE, (HWND)-3
	qsAllInput    = 0x04FF
	pmRemove      = 1
	gmemMoveable  = 2
	maxClipText   = 1 << 20
	maxClipImage  = 16 << 20
)

var (
	cfPNG = registerFormat("PNG")
	// password managers mark secrets with this format so that clipboard
	// tools leave them alone
	cfExclude = registerFormat("ExcludeClipboardContentFromMonitorProcessing")
	// tells Explorer to copy, not move, pasted files
	cfDropEffect = registerFormat("Preferred DropEffect")
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

func openClipboard(hwnd uintptr) bool {
	for i := 0; i < 10; i++ {
		if r, _, _ := procOpenClipboard.Call(hwnd); r != 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// readClipboard returns the clipboard as files, PNG or text; ok is false
// when it holds none of them, holds a secret, or cannot be opened.
func readClipboard() (clipContent, bool) {
	if !openClipboard(0) {
		return clipContent{}, false
	}
	defer procCloseClipboard.Call()
	if cfExclude != 0 && formatAvailable(cfExclude) {
		return clipContent{}, false
	}
	if formatAvailable(cfHDrop) {
		if b := clipboardBytes(cfHDrop, 1<<20); b != nil {
			if paths := dropFiles(b); len(paths) > 0 {
				return clipContent{kind: "files", paths: paths}, true
			}
		}
	}
	if cfPNG != 0 && formatAvailable(cfPNG) {
		if b := clipboardBytes(cfPNG, maxClipImage); b != nil {
			return clipContent{kind: "png", data: b}, true
		}
	}
	if formatAvailable(cfUnicodeText) {
		if b := clipboardBytes(cfUnicodeText, 2*maxClipText); b != nil {
			u := unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), len(b)/2)
			if i := indexZero(u); i >= 0 {
				u = u[:i]
			}
			return clipContent{kind: "text", data: []byte(string(utf16.Decode(u)))}, true
		}
	}
	if formatAvailable(cfDIB) {
		if b := clipboardBytes(cfDIB, maxClipImage); b != nil {
			if p, err := dibToPNG(b); err == nil {
				return clipContent{kind: "png", data: p}, true
			}
		}
	}
	return clipContent{}, false
}

// dropFiles returns the paths of a CF_HDROP, a DROPFILES structure.
func dropFiles(b []byte) []string {
	if len(b) < 20 {
		return nil
	}
	off := int(binary.LittleEndian.Uint32(b[0:4]))
	if binary.LittleEndian.Uint32(b[16:20]) == 0 || off < 20 || off >= len(b) {
		return nil // ANSI paths, which Explorer no longer writes
	}
	u := make([]uint16, (len(b)-off)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[off+2*i:])
	}
	var paths []string
	for len(u) > 0 && u[0] != 0 {
		i := indexZero(u)
		if i < 0 {
			break
		}
		paths = append(paths, string(utf16.Decode(u[:i])))
		u = u[i+1:]
	}
	return paths
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

// runClipboardBridge carries what Xpra's clipboard does not, while the
// Xpra client of vm runs; it returns when the client exits.
//
// Host to VM: it copies the host clipboard into the VM while one of its
// windows is in the foreground: when such a window gains focus, and when
// the clipboard changes while it has it. Content the Xpra client or the
// bridge put on the clipboard came from the VM and is not sent back.
//
// VM to host, files only, as Xpra carries text and images: when the VM's
// clipboard reaches the host, and when focus leaves the VM's windows, it
// fetches the files the VM's clipboard holds, if they changed, and puts
// them on the host clipboard for pasting in Explorer.
func runClipboardBridge(vm string, clientPID int, clientDir string, b clipBridge) error {
	runtime.LockOSThread()
	proc, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(clientPID))
	if err != nil {
		return fmt.Errorf("watch xpra client: %w", err)
	}
	defer windows.CloseHandle(proc)
	// owns the clipboard while it holds files from the VM
	static, _ := syscall.UTF16PtrFromString("STATIC")
	hwnd, _, err := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(static)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, 0, 0)
	if hwnd == 0 {
		return fmt.Errorf("create clipboard window: %v", err)
	}
	defer procDestroyWindow.Call(hwnd)
	suffix := " [" + vm + "]"
	var focused bool
	var seenSeq uint32
	var sent [32]byte
	var fetched string
	for {
		// other programs message the clipboard owner and wait for it
		h := proc
		r, _, _ := procMsgWaitForMultiple.Call(1, uintptr(unsafe.Pointer(&h)), 0, 150, qsAllInput)
		if r == 0 {
			return nil
		}
		pumpMessages()
		fg, _, _ := procGetForegroundWindow.Call()
		nowFocused := fg != 0 && strings.HasSuffix(windowTitle(fg), suffix) && inDir(windowProcess(fg), clientDir)
		seq := clipboardSeq()
		owner, _, _ := procGetClipboardOwner.Call()
		fromVM := owner == hwnd || inDir(windowProcess(owner), clientDir)
		if b.toGuest && nowFocused && (!focused || seq != seenSeq) && !fromVM {
			if c, ok := readClipboard(); ok {
				if sum := c.sum(); sum != sent {
					if err := b.send(c); err != nil {
						fmt.Fprintf(os.Stderr, "clipboard to %s: %v\n", vm, err)
					} else {
						sent = sum
					}
				}
			}
		}
		if b.toHost && ((seq != seenSeq && fromVM && owner != hwnd) || (focused && !nowFocused)) {
			key, err := fetchVMFiles(b, hwnd, fetched)
			if err != nil {
				fmt.Fprintf(os.Stderr, "files from %s: %v\n", vm, err)
			} else if key != "" {
				fetched = key
			}
			seq = clipboardSeq()
		}
		focused, seenSeq = nowFocused, seq
	}
}

func pumpMessages() {
	var msg [48]byte
	for {
		r, _, _ := procPeekMessage.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0, pmRemove)
		if r == 0 {
			return
		}
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg[0])))
	}
}

// fetchVMFiles copies the files on the VM's clipboard into a new folder
// under b.stage and puts them on the host clipboard, unless they are the
// ones fetched last, under key have. It returns the key of what it put
// there, or "" for nothing.
func fetchVMFiles(b clipBridge, hwnd uintptr, have string) (string, error) {
	s, err := b.openFiles()
	if err != nil {
		return "", err
	}
	defer s.Close()
	r := bufio.NewReader(s)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read file list: %w", err)
	}
	key := strings.TrimSpace(line)
	if key == "" || key == have {
		s.Write([]byte("n"))
		return "", nil
	}
	if _, err := s.Write([]byte("y")); err != nil {
		return "", err
	}
	if err := os.MkdirAll(b.stage, 0o700); err != nil {
		return "", err
	}
	// only the latest copy stays
	if old, err := os.ReadDir(b.stage); err == nil {
		for _, e := range old {
			os.RemoveAll(filepath.Join(b.stage, e.Name()))
		}
	}
	dir, err := os.MkdirTemp(b.stage, "")
	if err != nil {
		return "", err
	}
	tops, err := extractClipFiles(r, dir, os.Stderr)
	if err == nil && len(tops) == 0 {
		err = fmt.Errorf("no files arrived")
	}
	if err == nil {
		err = setClipboardFiles(hwnd, tops)
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return key, nil
}

// setClipboardFiles puts paths on the clipboard as Explorer copies files,
// owned by hwnd.
func setClipboardFiles(hwnd uintptr, paths []string) error {
	// DROPFILES: offset of the list, a point, two flags; then UTF-16
	// paths, each ending in NUL, and a final NUL
	var list []uint16
	for _, p := range paths {
		list = append(list, utf16.Encode([]rune(p))...)
		list = append(list, 0)
	}
	list = append(list, 0)
	buf := make([]byte, 20+2*len(list))
	binary.LittleEndian.PutUint32(buf[0:], 20)
	binary.LittleEndian.PutUint32(buf[16:], 1)
	for i, c := range list {
		binary.LittleEndian.PutUint16(buf[20+2*i:], c)
	}
	effect := make([]byte, 4)
	binary.LittleEndian.PutUint32(effect, 1) // DROPEFFECT_COPY
	if !openClipboard(hwnd) {
		return fmt.Errorf("the clipboard is busy")
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	if err := setClipboardBytes(cfHDrop, buf); err != nil {
		return err
	}
	if cfDropEffect != 0 {
		setClipboardBytes(cfDropEffect, effect)
	}
	return nil
}

func setClipboardBytes(format uintptr, b []byte) error {
	h, _, err := procGlobalAlloc.Call(gmemMoveable, uintptr(len(b)))
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %v", err)
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("GlobalLock failed")
	}
	procMoveMemory.Call(p, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	procGlobalUnlock.Call(h)
	if r, _, err := procSetClipboardData.Call(format, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData: %v", err)
	}
	return nil
}
