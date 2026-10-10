package image

import "testing"

func TestRewriteProxyURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8080":           "http://10.0.2.2:8080",
		"http://user:pass@localhost:8081": "http://user:pass@10.0.2.2:8081",
		"socks5://[::1]:1080":             "socks5://[10.0.2.2]:1080",
		"127.0.0.1:8080":                  "http://10.0.2.2:8080",
		"http://[::1]":                    "http://[10.0.2.2]",
		"https://proxy.example:443":       "https://proxy.example:443",
		"":                                "",
	}
	for input, want := range cases {
		if got := rewriteProxyURL(input); got != want {
			t.Errorf("rewriteProxyURL(%q) = %q, want %q", input, got, want)
		}
	}
}
