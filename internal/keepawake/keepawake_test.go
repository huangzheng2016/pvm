package keepawake

import "testing"

func TestManagerAcquireReleaseIdempotent(t *testing.T) {
	m := NewManager()
	m.SetActive(true)
	m.SetActive(true)
	m.SetActive(false)
	m.SetActive(false)
	m.Close()
}
