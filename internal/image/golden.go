package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

// Golden images are immutable assets: old versions are never modified in place.
type GoldenEntry struct {
	Version      int       `json:"version"`
	Distro       string    `json:"distro"`
	ImageVersion string    `json:"image_version"`
	File         string    `json:"file"` // relative to the golden dir
	BaseDigest   string    `json:"base_digest"`
	BuiltAt      time.Time `json:"built_at"`
	Packages     []string  `json:"packages"`
	DockerImages []string  `json:"docker_images"`
}

func GoldenDir(imagesDir string) string { return filepath.Join(imagesDir, "golden") }

func goldenCatalogPath(imagesDir string) string {
	return filepath.Join(GoldenDir(imagesDir), "golden.json")
}

func goldenFileName(distro, imageVersion string, v int) string {
	return fmt.Sprintf("%s-%s-v%d.qcow2", distro, imageVersion, v)
}

type goldenCatalog struct {
	Entries []GoldenEntry `json:"entries"`
}

func loadGoldenCatalog(imagesDir string) (*goldenCatalog, error) {
	data, err := os.ReadFile(goldenCatalogPath(imagesDir))
	if os.IsNotExist(err) {
		return &goldenCatalog{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c goldenCatalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("golden catalog: %w", err)
	}
	return &c, nil
}

func (c *goldenCatalog) save(imagesDir string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(GoldenDir(imagesDir), 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWrite(goldenCatalogPath(imagesDir), data, 0o644)
}

func (c *goldenCatalog) match(distro, imageVersion string) []GoldenEntry {
	var out []GoldenEntry
	for _, e := range c.Entries {
		if e.Distro == distro && e.ImageVersion == imageVersion {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out
}

func LatestGolden(imagesDir, distro, imageVersion string) *GoldenEntry {
	cat, err := loadGoldenCatalog(imagesDir)
	if err != nil {
		return nil
	}
	for _, e := range cat.match(distro, imageVersion) {
		if _, err := os.Stat(filepath.Join(GoldenDir(imagesDir), e.File)); err == nil {
			e := e
			return &e
		}
	}
	return nil
}

func ListGolden(imagesDir string) []GoldenEntry {
	cat, err := loadGoldenCatalog(imagesDir)
	if err != nil {
		return nil
	}
	out := cat.Entries
	sort.Slice(out, func(i, j int) bool {
		if out[i].Distro != out[j].Distro {
			return out[i].Distro < out[j].Distro
		}
		return out[i].Version > out[j].Version
	})
	return out
}
