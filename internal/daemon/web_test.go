package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDashboardReadOnlyHostAndTokenGuards(t *testing.T) {
	s, _, _ := newMountTestServer(t)
	rr := httptest.NewRecorder()
	s.handleDashboardOpen(rr, httptest.NewRequest("POST", "/v1/dashboard", nil))
	if rr.Code != 200 {
		t.Fatalf("open dashboard = %d", rr.Code)
	}
	var opened struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(opened.URL)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(u.Fragment, "token=")
	base := "http://" + u.Host

	// POST is rejected outright (no mutations exposed).
	resp, err := http.Post(base+"/api/snapshot", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}

	// DNS-rebinding Host header is rejected even with a valid token.
	req, _ := http.NewRequest("GET", base+"/api/snapshot", nil)
	req.Host = "evil.example.com"
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host status = %d, want 403", resp.StatusCode)
	}

	// Missing/wrong token -> 401; valid token -> 200 with CSP.
	resp, err = http.Get(base + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", base+"/api/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") || !strings.Contains(string(body), `"clusters"`) {
		t.Fatalf("authorized snapshot = %d, headers=%v, body=%s", resp.StatusCode, resp.Header, body)
	}
}
