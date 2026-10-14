package daemon

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Stages are in-memory progress, not an audit trail.
type StageEvent struct {
	Stage  string    `json:"stage"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

func (s *Server) stage(vmName, stage, detail string) {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	ev := StageEvent{Stage: stage, Detail: detail, At: time.Now()}
	evs := append(s.prog[vmName], ev)
	if len(evs) > 64 {
		evs = evs[len(evs)-64:]
	}
	s.prog[vmName] = evs
}

func (s *Server) stages(vmName string) []StageEvent {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	return append([]StageEvent(nil), s.prog[vmName]...)
}

func (s *Server) resetStages(vmName string) {
	s.progMu.Lock()
	delete(s.prog, vmName)
	s.progMu.Unlock()
}

func (s *Server) vmReady(vmName string) bool {
	for _, ev := range s.stages(vmName) {
		if ev.Stage == "ready" {
			return true
		}
	}
	return false
}

// The file size lets followers detect new output across tail windows.
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.getSpec(name); err != nil {
		writeErr(w, 404, err)
		return
	}
	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			tail = n
		}
	}
	logPath := s.P.VMDir(name) + "/console.log"
	lines, size, err := tailLines(logPath, tail)
	if err != nil {
		// Console file appears only once QEMU starts; an absent file is
		// not an error for follow mode, just an empty page.
		if os.IsNotExist(err) {
			writeJSON(w, 200, map[string]any{"lines": []string{}, "size": 0})
			return
		}
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"lines": lines, "size": size})
}

// Console logs grow unbounded, so read only a bounded tail window.
func tailLines(path string, n int) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := fi.Size()
	const maxRead = 256 * 1024
	start := int64(0)
	if size > maxRead {
		start = size - maxRead
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}
	text := string(buf)
	if start > 0 {
		// Drop the partial first line.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, size, nil
}
