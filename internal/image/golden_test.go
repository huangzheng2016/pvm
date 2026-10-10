package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenVersionIncrementAndLatest(t *testing.T) {
	dir := t.TempDir()
	mk := func(e GoldenEntry) {
		cat, err := loadGoldenCatalog(dir)
		if err != nil {
			t.Fatal(err)
		}
		cat.Entries = append(cat.Entries, e)
		if err := cat.save(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(GoldenDir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(GoldenDir(dir), e.File), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if g := LatestGolden(dir, "ubuntu", "24.04", "arm64"); g != nil {
		t.Fatalf("expected nil, got %+v", g)
	}
	mk(GoldenEntry{Version: 1, Distro: "ubuntu", ImageVersion: "24.04", Arch: "arm64", File: goldenFileName("ubuntu", "24.04", 1)})
	mk(GoldenEntry{Version: 2, Distro: "ubuntu", ImageVersion: "24.04", Arch: "arm64", File: goldenFileName("ubuntu", "24.04", 2)})
	mk(GoldenEntry{Version: 1, Distro: "ubuntu", ImageVersion: "24.04", Arch: "amd64", File: "ubuntu-24.04-amd64-v1.qcow2"})

	g := LatestGolden(dir, "ubuntu", "24.04", "arm64")
	if g == nil || g.Version != 2 {
		t.Fatalf("want v2, got %+v", g)
	}
	// version increment mirrors what Bake computes
	cat, _ := loadGoldenCatalog(dir)
	next := 1
	if ms := cat.match("ubuntu", "24.04", "arm64"); len(ms) > 0 {
		next = ms[0].Version + 1
	}
	if next != 3 {
		t.Fatalf("next version want 3, got %d", next)
	}
	// other arch unaffected
	g = LatestGolden(dir, "ubuntu", "24.04", "amd64")
	if g == nil || g.Version != 1 {
		t.Fatalf("amd64 want v1, got %+v", g)
	}
}

func TestLatestGoldenSkipsMissingFile(t *testing.T) {
	dir := t.TempDir()
	cat := &goldenCatalog{Entries: []GoldenEntry{
		{Version: 2, Distro: "ubuntu", ImageVersion: "24.04", Arch: "arm64", File: goldenFileName("ubuntu", "24.04", 2)},
		{Version: 1, Distro: "ubuntu", ImageVersion: "24.04", Arch: "arm64", File: goldenFileName("ubuntu", "24.04", 1)},
	}}
	if err := os.MkdirAll(GoldenDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cat.save(dir); err != nil {
		t.Fatal(err)
	}
	// only v1 exists on disk
	os.WriteFile(filepath.Join(GoldenDir(dir), goldenFileName("ubuntu", "24.04", 1)), []byte("x"), 0o644)
	g := LatestGolden(dir, "ubuntu", "24.04", "arm64")
	if g == nil || g.Version != 1 {
		t.Fatalf("want fallback to v1, got %+v", g)
	}
}

func TestRenderProvision(t *testing.T) {
	s, err := renderProvision("ubuntu", "baker")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"apt-get -y install docker.io qemu-guest-agent",
		"core-ubuntu-jammy:1.16.0", // present on every arch's candidate list
		"/etc/pvm-preinstalled-images",
		"cloud-init clean",
		"fonts-noto-cjk",
		"rm -f /etc/ssh/ssh_host_*",
		"truncate -s 0 /etc/machine-id",
		"sed -i '/^pvmshare /d' /etc/fstab",
		"userdel -rf baker",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("provision script missing %q", want)
		}
	}
	if strings.Contains(s, "docker system prune -af") {
		t.Error("provision script would prune the kasm image it just pulled")
	}
	if _, err := renderProvision("alpine", "baker"); err == nil {
		t.Error("expected error for alpine")
	}
	if _, err := renderProvision("ubuntu", "root"); err == nil {
		t.Error("expected error for root build user")
	}
}

func TestParsePreinstalled(t *testing.T) {
	log := "+ docker pull kasmweb/desktop:1.16.0\n" +
		"PVM_PREINSTALLED_BEGIN\n" +
		"kasmweb/desktop:1.16.0\n" +
		"PVM_PREINSTALLED_END\n" +
		"+ apt-get clean\n"
	got := parsePreinstalled(log)
	if len(got) != 1 || got[0] != "kasmweb/desktop:1.16.0" {
		t.Fatalf("got %v", got)
	}
	if parsePreinstalled("no markers here") != nil {
		t.Fatal("expected nil for missing markers")
	}
}
