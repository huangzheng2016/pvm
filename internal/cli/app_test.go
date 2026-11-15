package cli

import (
	"flag"
	"slices"
	"testing"
)

func TestFlagsFirstLeavesBoolFlagsAlone(t *testing.T) {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	fs.String("name", "", "")
	fs.Bool("kiosk", false, "")
	got := flagsFirst(fs, []string{"dev1", "--kiosk", "https://example.com", "--name", "mail"})
	want := []string{"--kiosk", "--name", "mail", "--", "dev1", "https://example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("flagsFirst = %q, want %q", got, want)
	}
	if err := fs.Parse(got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fs.Args(), []string{"dev1", "https://example.com"}) {
		t.Fatalf("positionals = %q", fs.Args())
	}
}
