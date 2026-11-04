package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestUsageListsOnlyGuestCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help returned %d: %s", code, stderr.String())
	}
	for _, want := range []string{"agent", "map", "skill"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("usage does not list %q: %s", want, stdout.String())
		}
	}
	for _, unwanted := range []string{"security", "route", "exit", "daemon", "mount", "create", "ssh", "exec"} {
		if strings.Contains(stdout.String(), unwanted) {
			t.Errorf("usage lists host command %q: %s", unwanted, stdout.String())
		}
	}
}

func TestHostCommandsAreUnknown(t *testing.T) {
	for _, command := range []string{"security", "route", "exit", "daemon", "mount", "create", "ssh", "exec", "version"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run([]string{command}, &stdout, &stderr); code != 2 {
				t.Fatalf("%q returned %d, want 2", command, code)
			}
			if !strings.Contains(stderr.String(), "unknown command") {
				t.Fatalf("%q did not report an unknown command: %s", command, stderr.String())
			}
		})
	}
}

func TestUnknownCommandHelpDoesNotBypassWhitelist(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"security", "--help"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown command with --help returned %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("unexpected output: %s", stderr.String())
	}
}

func TestServiceAndSkillRejectExtraArguments(t *testing.T) {
	for _, command := range []string{"agent", "skill"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{command, "extra"}, &stdout, &stderr); code != 2 {
			t.Fatalf("%s extra argument returned %d, want 2", command, code)
		}
	}
}

func TestSkillPrintsGuestGuide(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"skill"}, &stdout, &stderr); code != 0 {
		t.Fatalf("skill returned %d: %s", code, stderr.String())
	}
	if stdout.String() != skillDoc {
		t.Fatal("skill output differs from the embedded guest guide")
	}
	for _, want := range []string{"pvm map add 8080:80", "pvm map ls", "pvm map rm 8080", "/usr/local/bin/pvm agent"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("guest guide omits %q", want)
		}
	}
	for _, unwanted := range []string{"pvm security", "pvm route", "github.com"} {
		if strings.Contains(stdout.String(), unwanted) {
			t.Errorf("guest guide contains %q", unwanted)
		}
	}
}
