package main

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/hosted-status-page/hsp-server-agent/protocol"
)

// osPretty returns a human-readable description of the operating system, for display
// only. It describes the machine's software, never a person, and callers must treat a
// failure to determine it the same as an empty Hostname: optional, never fatal to the
// sample.
//
// On Linux this reads PRETTY_NAME from /etc/os-release, the same source `hostnamectl`
// and most distro tooling uses. On macOS it shells out to sw_vers, which every macOS
// install ships. Anywhere else — or if either fails — it returns "".
func osPretty() string {
	var raw string
	switch runtime.GOOS {
	case "linux":
		raw = linuxPrettyName("/etc/os-release")
	case "darwin":
		raw = darwinPrettyName()
	}
	return capOSPretty(raw)
}

// capOSPretty trims and bounds a raw OS description to the protocol limit. Split out
// from osPretty so the cap itself can be tested without depending on what any real
// os-release file or sw_vers output happens to contain on the machine running the test.
func capOSPretty(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > protocol.MaxOSPrettyBytes {
		raw = raw[:protocol.MaxOSPrettyBytes]
	}
	return raw
}

// linuxPrettyName reads PRETTY_NAME from a shell-style KEY=value file, normally
// /etc/os-release. The path is a parameter so tests can point it at a fixture instead
// of requiring root to bind-mount over the real file.
//
// A minimal line parser is used rather than exec-ing a shell to source it: this file is
// attacker-adjacent input on a machine we run as a service, and shelling out to
// interpret it would be a needless injection surface for a display string.
func linuxPrettyName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || key != "PRETTY_NAME" {
			continue
		}
		return strings.Trim(value, `"`)
	}
	return ""
}

// darwinPrettyName shells out to sw_vers, which is a fixed system binary at a known
// path, not user-influenced input, so this is not the injection surface the
// os-release parser above is written to avoid.
func darwinPrettyName() string {
	name, err := exec.Command("sw_vers", "-productName").Output()
	if err != nil {
		return ""
	}
	version, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return strings.TrimSpace(string(name))
	}
	return strings.TrimSpace(string(name)) + " " + strings.TrimSpace(string(version))
}
