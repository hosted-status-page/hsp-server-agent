package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests rehearse a real release update end to end: two genuinely compiled agents
// (one stamped as the installed release, one as the candidate), a release endpoint that
// serves the candidate and its SHA256SUMS, and the real updater and smoke test, all aimed
// at a temporary install target. Nothing outside the temp directories is touched, and a
// stub `systemctl` first on PATH proves the updater never restarts the service.

const (
	e2eInstalled = "0.1.4"
	e2eCandidate = "0.1.5"
)

// buildAgent compiles this package's agent stamped with version, like the release build.
func buildAgent(t *testing.T, version string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "serveragent-"+version)
	cmd := exec.Command("go", "build", "-ldflags", "-X main.AgentVersion="+version, "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build agent %s: %v\n%s", version, err, b)
	}
	return out
}

// builtAgents caches the compiled releases (as bytes) across tests in this run.
var builtAgents = map[string][]byte{}

func agentBinary(t *testing.T, version string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds real agents; skipped with -short")
	}
	if b, ok := builtAgents[version]; ok {
		return b
	}
	b, err := os.ReadFile(buildAgent(t, version))
	if err != nil {
		t.Fatal(err)
	}
	builtAgents[version] = b
	return b
}

type e2e struct {
	*fixture
	conf        string
	restartMark string
	candidate   []byte
	installed   []byte
}

// newE2E installs a real v0.1.4 agent in a temp directory and serves a real candidate.
func newE2E(t *testing.T, advertised string) *e2e {
	t.Helper()
	installed, candidate := agentBinary(t, e2eInstalled), agentBinary(t, e2eCandidate)

	f := newFixture(t, e2eInstalled, advertised, false)
	f.rel.publish(e2eCandidate, candidate)
	if err := os.WriteFile(f.exe, installed, 0o755); err != nil {
		t.Fatal(err)
	}
	f.u.SmokeTest = runSmokeTest // the real one: execute the downloaded binary with -version
	f.u.ConfigCheck = nil

	// A systemctl stub that records any call, put first on PATH.
	bin := t.TempDir()
	mark := filepath.Join(bin, "systemctl-was-called")
	stub := "#!/bin/sh\necho \"$@\" >> " + mark + "\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	conf := filepath.Join(t.TempDir(), "serveragent.conf")
	body := "SP_ENDPOINT=" + f.srv.URL + "\nSP_SERVER_ID=s\nSP_INGEST_KEY=k\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &e2e{fixture: f, conf: conf, restartMark: mark, candidate: candidate, installed: installed}
}

// run goes through the same entry point as `serveragent --update`.
func (e *e2e) run() (code int, out, errOut string) {
	var o, eo bytes.Buffer
	code = runUpdateWith(e.conf, &o, &eo, func(u *Updater) {
		*u = *e.u
		u.Out, u.ErrOut = &o, &eo
	})
	return code, o.String(), eo.String()
}

// installedVersion runs the installed binary, which is the real proof of what is there.
func (e *e2e) installedVersion(t *testing.T) string {
	t.Helper()
	b, err := exec.Command(e.exe, "-version").Output()
	if err != nil {
		t.Fatalf("installed binary is not runnable: %v", err)
	}
	v, ok := parseVersionOutput(string(b))
	if !ok {
		t.Fatalf("unexpected -version output %q", b)
	}
	return v
}

func (e *e2e) assertNoRestart(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(e.restartMark); err == nil {
		t.Fatal("the updater invoked systemctl; it must never restart the service")
	}
}

// assertOldIntact: still the byte-identical installed release, runnable, nothing left behind.
func (e *e2e) assertOldIntact(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(e.exe)
	if err != nil || !bytes.Equal(got, e.installed) {
		t.Fatalf("installed binary changed or unreadable: %v", err)
	}
	if v := e.installedVersion(t); v != e2eInstalled {
		t.Fatalf("installed binary reports %s, want %s", v, e2eInstalled)
	}
	e.assertNoTemp(t)
	e.assertNoRestart(t)
}

func TestE2EUpdateFromInstalledToCandidate(t *testing.T) {
	e := newE2E(t, e2eCandidate)
	e.u.GOARCH = "arm64" // the artifact for this architecture, not another one
	e.rel.files["serveragent-"+e2eCandidate+"-linux-arm64"] = e.candidate
	sum := sha256.Sum256(e.candidate)
	e.rel.files["SHA256SUMS"] = append(e.rel.files["SHA256SUMS"], (hex.EncodeToString(sum[:]) + "  serveragent-" + e2eCandidate + "-linux-arm64\n")...)

	code, out, errOut := e.run()
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	// 1. detected the candidate, 6. reports it
	if !strings.Contains(out, e2eInstalled) || !strings.Contains(out, e2eCandidate) {
		t.Errorf("output does not report %s -> %s: %q", e2eInstalled, e2eCandidate, out)
	}
	// 2. downloaded the right architecture only, 3. fetched SHA256SUMS
	if e.rel.hitCount("/dist/serveragent/serveragent-"+e2eCandidate+"-linux-arm64") != 1 ||
		e.rel.hitCount("/dist/serveragent/serveragent-"+e2eCandidate+"-linux-amd64") != 0 ||
		e.rel.hitCount("/dist/serveragent/SHA256SUMS") != 1 {
		t.Errorf("unexpected downloads: %v", e.rel.hits)
	}
	// 4./5. the installed file is now the verified candidate, runnable, with its mode
	if v := e.installedVersion(t); v != e2eCandidate {
		t.Errorf("installed binary reports %s, want %s", v, e2eCandidate)
	}
	if got, _ := os.ReadFile(e.exe); !bytes.Equal(got, e.candidate) {
		t.Error("installed binary is not byte-identical to the published candidate")
	}
	if fi, err := os.Stat(e.exe); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("replacement lost its executable bit: %v %v", fi, err)
	}
	e.assertNoTemp(t)
	// 7. prints the restart command, 8. and does not run it
	if !strings.Contains(out, "sudo systemctl restart statuspage-serveragent") {
		t.Errorf("restart command missing from output: %q", out)
	}
	e.assertNoRestart(t)
}

func TestE2EAlreadyCurrentCandidate(t *testing.T) {
	e := newE2E(t, e2eInstalled) // the server still advertises what is installed
	code, out, _ := e.run()
	if code != 0 || !strings.Contains(strings.ToLower(out), "up to date") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if e.rel.hitCount("/dist/serveragent/SHA256SUMS") != 0 {
		t.Error("downloaded something although already current")
	}
	e.assertOldIntact(t)
}

func TestE2EChecksumMismatchLeavesInstalledAgentUsable(t *testing.T) {
	e := newE2E(t, e2eCandidate)
	e.rel.files["SHA256SUMS"] = []byte(strings.Repeat("0", 64) + "  serveragent-" + e2eCandidate + "-linux-" + fakeArch + "\n")
	code, _, errOut := e.run()
	if code != exitVerify {
		t.Fatalf("exit %d (%s), want %d", code, errOut, exitVerify)
	}
	e.assertOldIntact(t)
}

func TestE2EMissingArtifactLeavesInstalledAgentUsable(t *testing.T) {
	e := newE2E(t, e2eCandidate)
	delete(e.rel.files, "serveragent-"+e2eCandidate+"-linux-"+fakeArch)
	code, _, errOut := e.run()
	if code != exitRelease {
		t.Fatalf("exit %d (%s), want %d", code, errOut, exitRelease)
	}
	e.assertOldIntact(t)
}

// A published file that checksums fine but is the wrong release must not be installed.
func TestE2EWrongReleaseBinaryIsRejectedBySmokeTest(t *testing.T) {
	e := newE2E(t, e2eCandidate)
	e.rel.files = map[string][]byte{}
	e.rel.publish(e2eCandidate, e.installed) // valid checksum, but it reports 0.1.4
	code, _, errOut := e.run()
	if code != exitVerify {
		t.Fatalf("exit %d (%s), want %d", code, errOut, exitVerify)
	}
	e.assertOldIntact(t)
}

// With the service account unable to read the config, nothing is downloaded or replaced.
func TestE2EUnreadableConfigStopsBeforeAnyDownload(t *testing.T) {
	e := newE2E(t, e2eCandidate)
	pf := newPermFixture(t)
	pf.chmodConfigUnreadable()
	e.u.ConfigCheck = pf.check.Run

	code, _, errOut := e.run()
	if code != exitConfig {
		t.Fatalf("exit %d (%s), want %d", code, errOut, exitConfig)
	}
	if !strings.Contains(errOut, "nothing was changed") {
		t.Errorf("error does not say nothing was changed: %q", errOut)
	}
	if e.rel.hitCount("/dist/serveragent/SHA256SUMS") != 0 || e.rel.hitCount("/dist/serveragent/serveragent-"+e2eCandidate+"-linux-"+fakeArch) != 0 {
		t.Errorf("downloaded despite the failed config check: %v", e.rel.hits)
	}
	e.assertOldIntact(t)
}
