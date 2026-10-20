package daemon

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCompactHandlerRejectsUnconsumedSnapshot(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	manifest := []byte(`{"name":"dev1","consumed":false}`)
	if err := os.WriteFile(filepath.Join(s.P.VMDir(spec.Name), "snapshot.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/vms/dev1/compact", nil)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)
	if rr.Code != 409 {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
}
