package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) handleGatewayDiag(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeErr(w, 400, fmt.Errorf("invalid cluster id"))
		return
	}
	cl := s.Clusters.Get(id)
	if cl == nil {
		writeErr(w, 404, fmt.Errorf("cluster %d not found", id))
		return
	}
	spec, err := s.getSpec(cl.Gateway)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	out, err := s.sshRunCtx(ctx, spec, "sudo journalctl -u pvm-gateway -n 150 --no-pager 2>&1 || true")
	if err != nil && ctx.Err() != nil {
		writeErr(w, 504, fmt.Errorf("collect gateway logs: %w", ctx.Err()))
		return
	}
	writeJSON(w, 200, map[string]string{"gateway": cl.Gateway, "log": out})
}
