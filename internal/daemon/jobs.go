package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

const (
	JobRunning   = "running"
	JobCompleted = "completed"
	JobFailed    = "failed"

	maxRetainedJobs = 256
)

// Job tracks a long-running daemon operation so agents can wait or poll by ID
// instead of resubmitting an uncertain mutation.
type Job struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	VM        string    `json:"vm"`
	Status    string    `json:"status"`
	Stage     string    `json:"stage,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	Error     *APIError `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	done      chan struct{}
}

func (j *Job) snapshot() Job {
	cp := *j
	cp.done = nil
	return cp
}

func (s *Server) startJob(kind, vmName string) *Job {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	j := &Job{
		ID:        hex.EncodeToString(b),
		Kind:      kind,
		VM:        vmName,
		Status:    JobRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		done:      make(chan struct{}),
	}
	s.jobsMu.Lock()
	if s.jobs == nil {
		s.jobs = map[string]*Job{}
		s.vmJob = map[string]string{}
	}
	s.jobs[j.ID] = j
	s.vmJob[vmName] = j.ID
	s.jobOrder = append(s.jobOrder, j.ID)
	for len(s.jobOrder) > maxRetainedJobs {
		old := s.jobOrder[0]
		s.jobOrder = s.jobOrder[1:]
		if s.jobs[old] != nil && s.jobs[old].Status != JobRunning {
			delete(s.jobs, old)
		}
	}
	s.jobsMu.Unlock()

	s.progMu.Lock()
	st := s.vmStatusLocked(vmName)
	st.CurrentJob = j.ID
	s.persistVMStatusLocked(vmName, false)
	s.progMu.Unlock()
	return j
}

func (s *Server) updateJob(vmName, stage, detail string) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if j := s.jobs[s.vmJob[vmName]]; j != nil && j.Status == JobRunning {
		j.Stage = stage
		j.Detail = detail
		j.UpdatedAt = time.Now()
	}
}

func (s *Server) finishJob(id string, err error) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	j := s.jobs[id]
	if j == nil || j.Status != JobRunning {
		return
	}
	j.UpdatedAt = time.Now()
	if err != nil {
		j.Status = JobFailed
		j.Error = classifyError(http.StatusInternalServerError, err)
	} else {
		j.Status = JobCompleted
		j.Stage = "ready"
	}
	close(j.done)
}

func (s *Server) getJob(id string) (*Job, bool) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	j := s.jobs[id]
	if j == nil {
		return nil, false
	}
	cp := j.snapshot()
	return &cp, true
}

func (s *Server) waitJob(id string, d time.Duration) (*Job, error) {
	s.jobsMu.Lock()
	j := s.jobs[id]
	s.jobsMu.Unlock()
	if j == nil {
		return nil, &APIError{Message: "job " + id + " not found", Code: "job_not_found", NextAction: "pvm job ls", HTTPStatus: http.StatusNotFound}
	}
	if d <= 0 || j.Status != JobRunning {
		cp := j.snapshot()
		return &cp, nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-j.done:
		cp := j.snapshot()
		return &cp, nil
	case <-timer.C:
		return nil, &APIError{
			Message:    "job " + id + " still running after " + d.String(),
			Code:       "wait_timeout",
			NextAction: "pvm job " + id + " --wait",
			Retryable:  true,
			HTTPStatus: http.StatusGatewayTimeout,
		}
	}
}

func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	vmFilter := r.URL.Query().Get("vm")
	s.jobsMu.Lock()
	out := []Job{}
	for i := len(s.jobOrder) - 1; i >= 0; i-- {
		if j := s.jobs[s.jobOrder[i]]; j != nil && (vmFilter == "" || j.VM == vmFilter) {
			out = append(out, j.snapshot())
		}
	}
	s.jobsMu.Unlock()
	writeJSON(w, 200, out)
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 600 {
				n = 600
			}
			wait = time.Duration(n) * time.Second
		}
	}
	j, err := s.waitJob(id, wait)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			writeErr(w, ae.HTTPStatus, ae)
			return
		}
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, j)
}
