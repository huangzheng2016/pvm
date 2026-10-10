package keys

import "testing"

func TestDebugEnsure(t *testing.T) {
	got, err := Ensure("/Users/user/.pvm/keys/id_ed25519")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ensure=%q", got)
}
