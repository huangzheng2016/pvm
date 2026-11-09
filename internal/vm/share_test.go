package vm

import "testing"

func TestPreparePrivateShare(t *testing.T) {
	if err := preparePrivateShare(t.TempDir(), 1000); err != nil {
		t.Fatal(err)
	}
}
