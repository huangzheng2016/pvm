package exit

import (
	"strings"
	"testing"
)

func TestEntryNameValidation(t *testing.T) {
	dir := t.TempDir()
	e := &Entry{Name: "us", Server: "1.2.3.4:18443"}
	if err := SaveEntry(dir, e); err != nil {
		t.Fatalf("save valid entry: %v", err)
	}
	if _, err := LoadEntry(dir, "us"); err != nil {
		t.Fatalf("load valid entry: %v", err)
	}

	for _, bad := range []string{"../x", "a/b", `..`, "", "-x", "x y", strings.Repeat("a", 40)} {
		if err := SaveEntry(dir, &Entry{Name: bad}); err == nil {
			t.Errorf("SaveEntry(%q) should be rejected", bad)
		}
		if _, err := LoadEntry(dir, bad); err == nil {
			t.Errorf("LoadEntry(%q) should be rejected", bad)
		}
		if err := RemoveEntry(dir, bad); err == nil {
			t.Errorf("RemoveEntry(%q) should be rejected", bad)
		}
	}
	if err := RemoveEntry(dir, "us"); err != nil {
		t.Fatalf("remove valid entry: %v", err)
	}
}
