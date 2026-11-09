package vm

import "testing"

func TestSyncDirectory(t *testing.T) {
	if err := syncDirectory(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
