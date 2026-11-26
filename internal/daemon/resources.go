package daemon

import (
	"context"
	"encoding/json"
	"log"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

const guestMemProbeScript = `import json
mem = {}
for line in open("/proc/meminfo"):
    k, v = line.split(":", 1)
    mem[k] = int(v.strip().split()[0]) * 1024
psi_some = 0.0
try:
    for line in open("/proc/pressure/memory"):
        if line.startswith("some "):
            for f in line.split():
                if f.startswith("avg10="):
                    psi_some = float(f.split("=")[1])
except FileNotFoundError:
    pass
oom = 0
for line in open("/proc/vmstat"):
    if line.startswith("oom_kill "):
        oom = int(line.split()[1])
print(json.dumps({
    "available": mem.get("MemAvailable", 0),
    "psi_some": psi_some,
    "oom_kills": oom,
    "boot_id": open("/proc/sys/kernel/random/boot_id").read().strip(),
}))
`

type balloonSample struct {
	Available uint64  `json:"available"`
	PSISome   float64 `json:"psi_some"`
	OOMKills  uint64  `json:"oom_kills"`
	BootID    string  `json:"boot_id"`
	ProbeOK   bool    `json:"-"`
}

type balloonState struct {
	current   uint64
	limit     uint64
	min       uint64
	bootID    string
	lastOOM   uint64
	idleSince time.Time
}

func newBalloonState(limit uint64) *balloonState {
	minBytes := uint64(512) << 20
	if limit/4 > minBytes {
		minBytes = limit / 4
	}
	return &balloonState{current: limit, limit: limit, min: minBytes}
}

// decideBalloon grows aggressively under guest memory pressure or probe
// failure, and reclaims idle memory slowly (1/4 per step, at most every 30s).
func decideBalloon(now time.Time, s *balloonState, in balloonSample) uint64 {
	if !in.ProbeOK {
		s.idleSince = time.Time{}
		s.current = s.limit
		return s.current
	}
	if in.BootID != "" && in.BootID != s.bootID {
		s.bootID = in.BootID
		s.lastOOM = in.OOMKills
		s.idleSince = time.Time{}
		s.current = s.limit
		return s.current
	}
	if in.OOMKills > s.lastOOM {
		s.lastOOM = in.OOMKills
		s.idleSince = time.Time{}
		s.current = s.limit
		return s.current
	}
	s.lastOOM = in.OOMKills
	const step = uint64(256) << 20
	pressured := in.PSISome >= 1.0 || in.Available < s.current*15/100
	if pressured {
		s.idleSince = time.Time{}
		grow := s.current / 2
		if grow < step {
			grow = step
		}
		next := s.current + grow
		if next > s.limit || next < s.current {
			next = s.limit
		}
		s.current = next
		return s.current
	}
	idle := in.PSISome == 0 && in.Available > s.current*60/100 && s.current > s.min
	if !idle {
		s.idleSince = time.Time{}
		return s.current
	}
	if s.idleSince.IsZero() {
		s.idleSince = now
		return s.current
	}
	if now.Sub(s.idleSince) < 30*time.Second {
		return s.current
	}
	s.idleSince = now
	excess := s.current - s.min
	shrink := excess / 4
	if shrink < step && excess >= step {
		shrink = step
	}
	if shrink > excess {
		shrink = excess
	}
	s.current -= shrink
	return s.current
}

func (s *Server) watchBalloon(spec *vm.Spec, seq uint64) {
	if !spec.Balloon || spec.Role != "user" || runtime.GOOS == "windows" {
		return
	}
	go func() {
		limit := uint64(spec.MemMB) << 20
		st := newBalloonState(limit)
		applied := limit
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if s.currentWatchSeq(spec.Name) != seq {
				return
			}
			if running, err := s.instance(spec).Running(); err != nil || running == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			out, code, err := s.runGuestCapture(ctx, spec.Name, []string{"python3", "-c", guestMemProbeScript}, "", 8)
			cancel()
			sample := balloonSample{}
			if err == nil && code == 0 && json.Unmarshal([]byte(strings.TrimSpace(out)), &sample) == nil {
				sample.ProbeOK = true
			}
			target := decideBalloon(time.Now(), st, sample)
			if target == applied {
				continue
			}
			q, err := qemu.DialQMP(s.P.QMPSock(spec.Name))
			if err != nil {
				continue
			}
			err = q.SetBalloonTarget(target)
			q.Close()
			if err != nil {
				log.Printf("vm %s: balloon target %s MiB failed: %v", spec.Name, strconv.FormatUint(target>>20, 10), err)
				continue
			}
			applied = target
		}
	}()
}
