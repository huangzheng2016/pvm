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
	if s.prog == nil {
		s.prog = make(map[string][]StageEvent)
	}
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
	if task := s.installTasks[vmName]; task != nil {
		task.cancel()
		delete(s.installTasks, vmName)
	}
	delete(s.prog, vmName)
	if s.progSeq == nil {
		s.progSeq = make(map[string]uint64)
	}
	s.progSeq[vmName]++
	s.progMu.Unlock()
}

func (s *Server) progressGeneration(name string) uint64 {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	return s.progSeq[name]
}

func (s *Server) stageIfCurrent(name string, seq uint64, stage, detail string) {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	if s.progSeq[name] != seq {
		return
	}
	if s.prog == nil {
		s.prog = make(map[string][]StageEvent)
	}
	events := append(s.prog[name], StageEvent{Stage: stage, Detail: detail, At: time.Now()})
	if len(events) > 64 {
		events = events[len(events)-64:]
	}
	s.prog[name] = events
}

func (s *Server) vmReady(vmName string) bool {
	events := s.stages(vmName)
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Stage {
		case "ready":
			return true
		case "failed", "provision", "ssh", "agent":
			return false
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
