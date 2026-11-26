// Package netcheck implements active, evidence-based egress and isolation
// checks shared by the host daemon, gateway and CLI.
package netcheck

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/netutil"
)

type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

type EchoService struct {
	Name  string
	URL   string
	Trace bool // parse ip= from cloudflare cdn-cgi/trace
}

func DefaultEchoServices() []EchoService {
	return []EchoService{
		{"icanhazip", "https://ipv4.icanhazip.com/", false},
		{"aws-checkip", "https://checkip.amazonaws.com/", false},
		{"akamai", "https://whatismyip.akamai.com/", false},
		{"cloudflare-trace", "https://1.1.1.1/cdn-cgi/trace", true},
		{"ipify", "https://api.ipify.org/", false},
		{"geojs", "https://ipv4.geojs.io/v1/ip", false},
	}
}

// ProbeExitIP queries independent echo services with a 1s stagger: a slow
// service does not block faster ones, and a failed service fails over at once.
// Only a valid public IPv4 address is accepted.
func ProbeExitIP(ctx context.Context, dial DialContextFunc, services []EchoService) (string, string, error) {
	if len(services) == 0 {
		services = DefaultEchoServices()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	type result struct {
		ip, svc string
		err     error
	}
	results := make(chan result, len(services))
	launched, completed := 0, 0
	var lastErr error
	for launched < len(services) {
		if launched > 0 {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				goto drain
			case r := <-results:
				timer.Stop()
				completed++
				if r.err == nil && r.ip != "" {
					return r.ip, r.svc, nil
				}
				lastErr = r.err
				// Failed fast: start the next provider without waiting the full stagger.
			case <-timer.C:
			}
		}
		svc := services[launched]
		launched++
		go func(s EchoService) {
			ip, err := fetchExitIPv4(ctx, dial, s)
			results <- result{ip: ip, svc: s.Name, err: err}
		}(svc)
	}
drain:
	for completed < launched {
		select {
		case r := <-results:
			completed++
			if r.err == nil && r.ip != "" {
				return r.ip, r.svc, nil
			}
			lastErr = r.err
		case <-ctx.Done():
			return "", "", fmt.Errorf("exit IP probe canceled: %w (last: %v)", ctx.Err(), lastErr)
		}
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return "", "", fmt.Errorf("all exit IP echo services failed: %w", lastErr)
}

func fetchExitIPv4(ctx context.Context, dial DialContextFunc, svc EchoService) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, svc.URL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Transport: &http.Transport{DialContext: dial, ForceAttemptHTTP2: false},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("redirect refused")
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", svc.Name, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil {
		return "", err
	}
	if len(body) > 4096 {
		return "", fmt.Errorf("%s: oversized response", svc.Name)
	}
	candidate := strings.TrimSpace(string(body))
	if svc.Trace {
		candidate = ""
		for _, line := range strings.Split(string(body), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ip="); ok {
				if candidate != "" {
					return "", fmt.Errorf("%s: duplicate ip lines", svc.Name)
				}
				candidate = v
			}
		}
	}
	ip := net.ParseIP(candidate)
	if ip == nil || ip.To4() == nil || netutil.IsBlockedEgressIP(ip) {
		return "", fmt.Errorf("%s: invalid public IPv4 %q", svc.Name, candidate)
	}
	return ip.String(), nil
}

type GeoInfo struct {
	IP          string `json:"ip"`
	CountryCode string `json:"country_code"`
	Region      string `json:"region,omitempty"`
	City        string `json:"city,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
	Source      string `json:"source,omitempty"`
}

// LookupGeo cross-checks the exit IP's country/region/timezone over the same exit.
func LookupGeo(ctx context.Context, dial DialContextFunc, ip string) (*GeoInfo, error) {
	client := &http.Client{Transport: &http.Transport{DialContext: dial, ForceAttemptHTTP2: false}}
	type geoSource struct {
		name, url string
		parse     func([]byte) (*GeoInfo, error)
	}
	sources := []geoSource{
		{"ipwho.is", "https://ipwho.is/" + ip, func(b []byte) (*GeoInfo, error) {
			var v struct {
				Success bool   `json:"success"`
				IP      string `json:"ip"`
				Country string `json:"country_code"`
				Region  string `json:"region"`
				City    string `json:"city"`
				TZ      struct {
					ID string `json:"id"`
				} `json:"timezone"`
			}
			if err := json.Unmarshal(b, &v); err != nil || !v.Success {
				return nil, fmt.Errorf("ipwho.is lookup failed")
			}
			return &GeoInfo{IP: v.IP, CountryCode: v.Country, Region: v.Region, City: v.City, Timezone: v.TZ.ID}, nil
		}},
		{"geojs", "https://get.geojs.io/v1/ip/geo/" + ip + ".json", func(b []byte) (*GeoInfo, error) {
			var v struct {
				IP      string `json:"ip"`
				Country string `json:"country_code"`
				Region  string `json:"region"`
				City    string `json:"city"`
				TZ      string `json:"timezone"`
			}
			if err := json.Unmarshal(b, &v); err != nil || v.Country == "" {
				return nil, fmt.Errorf("geojs lookup failed")
			}
			return &GeoInfo{IP: v.IP, CountryCode: v.Country, Region: v.Region, City: v.City, Timezone: v.TZ}, nil
		}},
		{"ipapi.co", "https://ipapi.co/" + ip + "/json/", func(b []byte) (*GeoInfo, error) {
			var v struct {
				IP      string `json:"ip"`
				Country string `json:"country_code"`
				Region  string `json:"region"`
				City    string `json:"city"`
				TZ      string `json:"timezone"`
				Error   bool   `json:"error"`
			}
			if err := json.Unmarshal(b, &v); err != nil || v.Error || v.Country == "" {
				return nil, fmt.Errorf("ipapi.co lookup failed")
			}
			return &GeoInfo{IP: v.IP, CountryCode: v.Country, Region: v.Region, City: v.City, Timezone: v.TZ}, nil
		}},
	}
	var lastErr error
	for _, src := range sources {
		reqCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, src.url, nil)
		if err != nil {
			cancel()
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		resp.Body.Close()
		cancel()
		if resp.StatusCode != 200 || err != nil {
			lastErr = fmt.Errorf("%s: http %d", src.name, resp.StatusCode)
			continue
		}
		g, err := src.parse(b)
		if err != nil || (g.IP != "" && g.IP != ip) || len(g.CountryCode) != 2 {
			lastErr = fmt.Errorf("%s: inconsistent geo response", src.name)
			continue
		}
		g.Source = src.name
		return g, nil
	}
	return nil, lastErr
}

// ExitPolicy binds an exit to a static IP or a dynamic geographic scope.
type ExitPolicy struct {
	Mode     string `json:"mode"`            // static | dynamic
	Scope    string `json:"scope,omitempty"` // city | region | country | any
	AnchorIP string `json:"anchor_ip,omitempty"`
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	City     string `json:"city,omitempty"`
}

func (p *ExitPolicy) Evaluate(ip string, geo *GeoInfo) error {
	if p == nil {
		return nil
	}
	if geo == nil || geo.CountryCode == "" {
		return fmt.Errorf("exit country could not be confirmed")
	}
	if strings.EqualFold(geo.CountryCode, "CN") {
		return fmt.Errorf("mainland China exits are blocked by policy")
	}
	if p.Mode == "static" {
		if p.AnchorIP != "" && ip != p.AnchorIP {
			return fmt.Errorf("exit IP drifted from pinned %s", p.AnchorIP)
		}
		return nil
	}
	scope := p.Scope
	if scope == "" {
		scope = "country"
	}
	switch scope {
	case "any":
		return nil
	case "country":
		if p.Country != "" && !strings.EqualFold(p.Country, geo.CountryCode) {
			return fmt.Errorf("exit country drifted from %s to %s", p.Country, geo.CountryCode)
		}
	case "region":
		if p.Country != "" && !strings.EqualFold(p.Country, geo.CountryCode) || p.Region != "" && !strings.EqualFold(p.Region, geo.Region) || geo.Region == "" {
			return fmt.Errorf("exit region drifted or is unknown")
		}
	case "city":
		if p.Country != "" && !strings.EqualFold(p.Country, geo.CountryCode) || p.Region != "" && !strings.EqualFold(p.Region, geo.Region) || p.City != "" && !strings.EqualFold(p.City, geo.City) || geo.City == "" {
			return fmt.Errorf("exit city drifted or is unknown")
		}
	}
	return nil
}

const (
	StatePassed  = "passed"
	StateFailed  = "failed"
	StateUnknown = "unknown"
)

type CheckItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Critical  bool      `json:"critical"`
	Evidence  string    `json:"evidence"`
	Remedy    string    `json:"remedy,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

type Report struct {
	Policy    string      `json:"policy"`
	RunID     string      `json:"run_id"`
	VM        string      `json:"vm"`
	BootID    string      `json:"vm_boot_id"`
	RouteHash string      `json:"route_hash"`
	CreatedAt time.Time   `json:"created_at"`
	Checks    []CheckItem `json:"checks"`
}

func NewReport(vmName, bootID, routeHash string) *Report {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &Report{
		Policy:    "pvm-check/1",
		RunID:     hex.EncodeToString(b[:]),
		VM:        vmName,
		BootID:    bootID,
		RouteHash: routeHash,
		CreatedAt: time.Now().UTC(),
	}
}

// Authorizes requires every critical check to have passed with non-empty
// evidence for the same boot and route generation within the last 24h.
func (r *Report) Authorizes(bootID, routeHash string) bool {
	if r == nil || r.Policy != "pvm-check/1" || r.VM == "" || len(r.Checks) == 0 {
		return false
	}
	if bootID != "" && r.BootID != bootID || routeHash != "" && r.RouteHash != routeHash {
		return false
	}
	if time.Since(r.CreatedAt) > 24*time.Hour {
		return false
	}
	for _, c := range r.Checks {
		if c.Critical && (c.State != StatePassed || strings.TrimSpace(c.Evidence) == "") {
			return false
		}
	}
	return true
}
