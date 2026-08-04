package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hosted-status-page/hsp-server-agent/protocol"
)

func TestLinuxPrettyNameParsesOSRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "os-release")
	content := "NAME=\"Ubuntu\"\n" +
		"VERSION=\"22.04.4 LTS (Jammy Jellyfish)\"\n" +
		"PRETTY_NAME=\"Ubuntu 22.04.4 LTS\"\n" +
		"ID=ubuntu\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if got := linuxPrettyName(path); got != "Ubuntu 22.04.4 LTS" {
		t.Errorf("got %q, want %q", got, "Ubuntu 22.04.4 LTS")
	}
}

func TestLinuxPrettyNameMissingFile(t *testing.T) {
	if got := linuxPrettyName(filepath.Join(t.TempDir(), "does-not-exist")); got != "" {
		t.Errorf("got %q, want empty for a missing file", got)
	}
}

func TestLinuxPrettyNameNoPrettyNameLine(t *testing.T) {
	// Alpine and some minimal distros omit PRETTY_NAME entirely; the agent must not
	// error or fall back to some other field, just report nothing.
	path := filepath.Join(t.TempDir(), "os-release")
	if err := os.WriteFile(path, []byte("NAME=\"Alpine Linux\"\nID=alpine\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if got := linuxPrettyName(path); got != "" {
		t.Errorf("got %q, want empty when PRETTY_NAME is absent", got)
	}
}

func TestLinuxPrettyNameHandlesUnquotedValue(t *testing.T) {
	// Not every distro quotes the value; some tooling downstream writes it bare.
	path := filepath.Join(t.TempDir(), "os-release")
	if err := os.WriteFile(path, []byte("PRETTY_NAME=Arch Linux\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if got := linuxPrettyName(path); got != "Arch Linux" {
		t.Errorf("got %q, want %q", got, "Arch Linux")
	}
}

// TestOSPrettyDoesNotPanic exercises the real code path on the machine actually running
// the test. It cannot assert a specific value — that depends on the CI/dev box — but it
// pins the two properties that matter regardless of platform: never panics or errors
// out of the batch, and respects the byte cap.
func TestOSPrettyDoesNotPanic(t *testing.T) {
	got := osPretty()
	if len(got) > protocol.MaxOSPrettyBytes {
		t.Errorf("osPretty() returned %d bytes, want at most %d", len(got), protocol.MaxOSPrettyBytes)
	}
	t.Logf("osPretty() on this machine: %q", got)
}

// TestCapOSPrettyTruncatesOverlongValue guards the cap independent of what any real
// os-release file or sw_vers output contains: a distro string this long would otherwise
// widen every ingest request that includes it.
func TestCapOSPrettyTruncatesOverlongValue(t *testing.T) {
	over := strings.Repeat("x", protocol.MaxOSPrettyBytes*2)
	got := capOSPretty(over)
	if len(got) != protocol.MaxOSPrettyBytes {
		t.Errorf("len = %d, want %d", len(got), protocol.MaxOSPrettyBytes)
	}
}

func TestCapOSPrettyTrimsWhitespace(t *testing.T) {
	if got := capOSPretty("  Ubuntu 22.04.4 LTS  \n"); got != "Ubuntu 22.04.4 LTS" {
		t.Errorf("got %q, want trimmed", got)
	}
}

func TestCapOSPrettyLeavesShortValuesAlone(t *testing.T) {
	if got := capOSPretty("Ubuntu 22.04.4 LTS"); got != "Ubuntu 22.04.4 LTS" {
		t.Errorf("got %q, want unchanged", got)
	}
}
