package cli

import "testing"

func TestResolveImageSpec(t *testing.T) {
	cases := []struct {
		name, image, distro, version, want string
	}{
		{"image only", "alpine:3.20", "", "", "alpine:3.20"},
		{"distro only", "", "alpine", "", "alpine"},
		{"distro and version", "", "alpine", "3.20", "alpine:3.20"},
		{"image with no version plus version", "alpine", "", "3.20", "alpine:3.20"},
		{"same version", "alpine:3.20", "", "3.20", "alpine:3.20"},
		{"same distro keeps version", "alpine:3.20", "alpine", "", "alpine:3.20"},
		{"version only errors", "", "", "3.20", ""},
		{"conflicting version", "alpine:3.21", "", "3.20", ""},
		{"conflicting distro", "ubuntu:24.04", "alpine", "", ""},
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
