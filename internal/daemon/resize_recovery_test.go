package daemon

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestResizeUsesActualDiskAndRecoversMetadataFailure(t *testing.T) {
	s, spec, _ := newMountTestServer(t)
	spec.DiskGB = 10
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}
	s.cacheSpec(spec)
	img := filepath.Join(t.TempDir(), "qemu-img")
	script := `#!/bin/sh
set -eu
case "$1" in
 info) printf '{"virtual-size":%s}\n' "$PVM_TEST_DISK_BYTES" ;;
 resize)
  printf resized > "$PVM_TEST_RESIZE_MARKER"
  if [ "${PVM_TEST_FAIL_SAVE:-0}" = 1 ]; then
   mv "$PVM_TEST_SPEC" "$PVM_TEST_SPEC.saved"
   mkdir "$PVM_TEST_SPEC"
  fi ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(img, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s.TC = &qemu.Toolchain{Img: img}
	marker := filepath.Join(t.TempDir(), "resize-called")
	t.Setenv("PVM_TEST_RESIZE_MARKER", marker)
	t.Setenv("PVM_TEST_SPEC", spec.SpecPath(s.P))
	t.Setenv("PVM_TEST_DISK_BYTES", "21474836480")
	resize := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("POST", "/v1/vms/dev1/resize", bytes.NewBufferString(body)))
		return rr
	}
	rr := resize(`{"size_gb":15}`)
	if rr.Code != 400 {
		t.Fatalf("shrink accepted: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("invoked resize for an actual shrink")
	}
	t.Setenv("PVM_TEST_FAIL_SAVE", "1")
	rr = resize(`{"size_gb":30}`)
	if rr.Code != 500 || !strings.Contains(rr.Body.String(), "retry") {
		t.Fatalf("missing partial failure recovery: %d %s", rr.Code, rr.Body.String())
	}
	cached, err := s.getSpec(spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if cached.DiskGB != 10 {
		t.Fatalf("failed save published size %d", cached.DiskGB)
	}
	if err := os.Remove(spec.SpecPath(s.P)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(spec.SpecPath(s.P)+".saved", spec.SpecPath(s.P)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PVM_TEST_FAIL_SAVE", "0")
	t.Setenv("PVM_TEST_DISK_BYTES", "32212254720")
	rr = resize(`{"size_gb":30}`)
	if rr.Code != 200 {
		t.Fatalf("retry: %d %s", rr.Code, rr.Body.String())
	}
	loaded, err := vm.Load(s.P, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DiskGB != 30 {
		t.Fatalf("metadata not repaired: %d", loaded.DiskGB)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("metadata retry unnecessarily resized disk")
	}
}
