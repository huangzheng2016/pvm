package cli

import "testing"

func TestResolveImageSpec(t *testing.T) {
	cases := []struct {
		name, image, distro, version, want string
	}{
		{"image only", "debian:13", "", "", "debian:13"},
		{"distro only", "", "debian", "", "debian"},
		{"distro and version", "", "debian", "13", "debian:13"},
		{"image with no version plus version", "debian", "", "13", "debian:13"},
		{"same version", "debian:13", "", "13", "debian:13"},
		{"same distro keeps version", "debian:13", "debian", "", "debian:13"},
		{"version only errors", "", "", "13", ""},
		{"conflicting version", "debian:13", "", "12", ""},
		{"conflicting distro", "ubuntu:24.04", "debian", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveImageSpec(tc.image, tc.distro, tc.version)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
