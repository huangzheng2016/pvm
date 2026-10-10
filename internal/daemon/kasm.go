package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/image"
)

const (
	kasmContainer = "pvm-kasm"
	kasmPort      = 6901
)

type kasmResponse struct {
	URL      string `json:"url"`
	HostPort int    `json:"host_port"`
}

// handleKasm idempotently (re)starts the kasm desktop container in the
// VM, waits for its HTTPS endpoint and publishes it through a cluster
// port map fronted by a credential-injecting loopback proxy, so the
// returned URL needs no login.
func (s *Server) handleKasm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		writeErr(w, 400, fmt.Errorf("vm %s is not a cluster vm", name))
		return
	}
	inst := s.instance(spec)
	if st, err := inst.Running(); err != nil {
		writeErr(w, 500, err)
		return
	} else if st == nil {
		writeErr(w, 409, fmt.Errorf("vm %s is not running", name))
		return
	}

	pw := randHex(8)
	run := fmt.Sprintf(
		`docker rm -f %[1]s >/dev/null 2>&1; `+
			`docker run -d --name %[1]s --shm-size=512m -e VNC_PW=%[2]s -p %[3]d:%[3]d %[4]s >/dev/null && echo STARTED`,
		kasmContainer, pw, kasmPort, image.KasmImage)
	out, err := s.sshRun(spec, run)
	if err != nil {
		writeErr(w, 500, fmt.Errorf("start kasm container: %v\n%s", err, out))
		return
	}

	// wait for the KasmVNC endpoint; 401 means it is up and demanding
	// the basic-auth credentials (VNC_PW), 200 that it let us straight in
	ready := false
	for i := 0; i < 40; i++ {
		out, err := s.sshRun(spec, fmt.Sprintf("curl -sk -o /dev/null -w %%{http_code} https://127.0.0.1:%d/", kasmPort))
		code := strings.TrimSpace(out)
		if err == nil && (code == "200" || code == "401") {
			ready = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !ready {
		writeErr(w, 500, fmt.Errorf("kasm did not become ready on :%d within 2min", kasmPort))
		return
	}

	// publish through the cluster map (reuse an existing one)
	hostPort := 0
	var poolPort int
	for _, m := range s.Clusters.Get(spec.ClusterID).Maps {
		if m.VM == spec.Name && m.GuestPort == kasmPort && m.Proto == "tcp" {
			hostPort, poolPort = m.HostPort, m.PoolPort
		}
	}
	if hostPort == 0 {
		port, err := freeLoopbackPort("tcp")
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		m, err := s.Clusters.AddMap(spec.ClusterID, spec.Name, spec.IP, port, kasmPort, "tcp")
		if err != nil {
			writeErr(w, 409, err)
			return
		}
		if err := s.relays.Ensure("tcp", m.HostPort, m.PoolPort); err != nil {
			s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, "tcp")
			writeErr(w, 500, err)
			return
		}
		if cl := s.Clusters.Get(spec.ClusterID); cl != nil {
			if err := s.pushGatewayConfig(cl); err != nil {
				writeErr(w, 500, err)
				return
			}
		}
		hostPort, poolPort = m.HostPort, m.PoolPort
	}
	_ = poolPort

	px, err := s.kasmProxyFor(spec.Name, hostPort, pw)
	if err != nil {
		writeErr(w, 500, fmt.Errorf("kasm proxy: %v", err))
		return
	}
	writeJSON(w, 200, kasmResponse{
		URL:      fmt.Sprintf("http://127.0.0.1:%d/", px.port),
		HostPort: px.port,
	})
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
