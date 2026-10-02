package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the config-access preflight in install.sh. The function is cut out of
// the script between its markers and run in a throwaway bash with stand-in `id`, `runuser`
// and `stat` commands, so they need neither root nor a real service account. Switching to a
// real unprivileged identity is covered by the Go-side TestAccessAsUserDeniesAnUnprivilegedAccount.

const installScript = "../../install.sh"

func installSource(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(installScript)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func preflightFunctions(t *testing.T) string {
	t.Helper()
	src := installSource(t)
	start := strings.Index(src, "# preflight:begin")
	end := strings.Index(src, "# preflight:end")
	if start < 0 || end < start {
		t.Fatal("install.sh lost its preflight markers")
	}
	return src[start:end]
}

type installHarness struct {
	t       *testing.T
	dir     string
	bin     string
	conf    string
	denied  []string // paths the stand-in runuser refuses
	runuser string   // body override for the stand-in runuser
}

func newInstallHarness(t *testing.T) *installHarness {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	h := &installHarness{t: t, dir: t.TempDir()}
	h.bin = filepath.Join(h.dir, "fakebin")
	if err := os.Mkdir(h.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	confDir := filepath.Join(h.dir, "etc", "statuspage")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.conf = filepath.Join(confDir, "serveragent.conf")
	h.write(h.conf, "SP_INGEST_KEY=secret-value\n")
	h.systemd("statuspage-agent", "statuspage-agent", h.conf)

	// id: only the canonical service account exists.
	h.script("id", `if [ "$1" = "-g" ]; then echo 1000; exit 0; fi
[ "$1" = "statuspage-agent" ]`)
	// stat -c '%U:%G %a': a fixed, secret-free description.
	h.script("stat", `echo "root:root 700"`)
	return h
}

// systemd installs a stand-in `systemctl show ... -p KEY --value` that reports the given
// effective identity and config path, in the format systemd really prints.
func (h *installHarness) systemd(user, group, config string) {
	h.t.Helper()
	h.script("systemctl", fmt.Sprintf(`[ "$1" = "show" ] || exit 2
case "$4" in
User) echo %q ;;
Group) echo %q ;;
ExecStart) echo "{ path=/usr/local/bin/serveragent ; argv[]=/usr/local/bin/serveragent -config %s ; ignore_errors=no ; start_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }" ;;
esac`, user, group, config))
}

func (h *installHarness) write(path, body string) {
	h.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		h.t.Fatal(err)
	}
}

func (h *installHarness) script(name, body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

// run executes the preflight and returns exit code and combined output.
func (h *installHarness) run(mode string) (int, string) {
	h.t.Helper()
	runuser := h.runuser
	if runuser == "" {
		// runuser -u USER -- test FLAG PATH: refuse any listed path, allow the rest.
		var cases []string
		for _, p := range h.denied {
			cases = append(cases, fmt.Sprintf("%q) exit 1 ;;", p))
		}
		runuser = fmt.Sprintf(`shift 3
case "$3" in
%s
esac
exit 0`, strings.Join(cases, "\n"))
	}
	h.script("runuser", runuser)

	harness := fmt.Sprintf(`set -euo pipefail
log()  { :; }
warn() { printf 'warning: %%s\n' "$*" >&2; }
die()  { printf 'error: %%s\n' "$*" >&2; exit 1; }
CONFIG_FILE=%q
SERVICE_USER="statuspage-agent"
%s
preflight_config_access %s
echo preflight-ok
`, h.conf, preflightFunctions(h.t), mode)

	cmd := exec.Command("bash", "-c", harness)
	cmd.Env = []string{"PATH=" + h.bin + ":/usr/bin:/bin"}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatal(err)
	}
	return code, out.String()
}

// tree captures everything under the harness directory so a test can prove nothing changed.
func (h *installHarness) tree() string {
	h.t.Helper()
	var b strings.Builder
	filepath.Walk(h.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || strings.HasPrefix(p, h.bin) {
			return nil
		}
		fmt.Fprintf(&b, "%s %v", p, info.Mode())
		if !info.IsDir() {
			body, _ := os.ReadFile(p)
			fmt.Fprintf(&b, " %q", body)
		}
		b.WriteString("\n")
		return nil
	})
	return b.String()
}

func TestInstallerPreflightAcceptsReadableConfig(t *testing.T) {
	h := newInstallHarness(t)
	before := h.tree()
	for _, mode := range []string{"upgrade", "install"} {
		code, out := h.run(mode)
		if code != 0 || !strings.Contains(out, "preflight-ok") || strings.Contains(out, "error") {
			t.Errorf("%s: code=%d out=%q", mode, code, out)
		}
	}
	if h.tree() != before {
		t.Error("the preflight changed the tree")
	}
}

func TestInstallerPreflightRejectsMissingConfig(t *testing.T) {
	h := newInstallHarness(t)
	os.Remove(h.conf)
	code, out := h.run("upgrade")
	if code == 0 || !strings.Contains(out, "does not exist") || !strings.Contains(out, "not touched") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestInstallerPreflightRejectsUnknownServiceUser(t *testing.T) {
	h := newInstallHarness(t)
	h.systemd("ghost", "ghost", h.conf)
	code, out := h.run("upgrade")
	if code == 0 || !strings.Contains(out, "'ghost'") || !strings.Contains(out, "does not exist") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

// Readable by root (the test process), refused to the service user.
func TestInstallerPreflightRejectsConfigUnreadableByServiceUser(t *testing.T) {
	h := newInstallHarness(t)
	h.denied = []string{h.conf}
	before := h.tree()
	code, out := h.run("upgrade")
	if code == 0 {
		t.Fatalf("expected failure, out=%q", out)
	}
	for _, want := range []string{"'statuspage-agent' cannot read " + h.conf, "chgrp statuspage-agent", "chmod 0640", "not touched"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "secret-value") || strings.Contains(out, "preflight-ok") {
		t.Errorf("leaked contents or continued: %q", out)
	}
	if h.tree() != before {
		t.Error("a failed preflight changed the tree")
	}
}

// The v0.1.4 rollout case: the file is fine but its directory cannot be entered.
func TestInstallerPreflightNamesUnsearchableDirectory(t *testing.T) {
	h := newInstallHarness(t)
	confDir := filepath.Dir(h.conf)
	h.denied = []string{h.conf, confDir}
	code, out := h.run("upgrade")
	if code == 0 {
		t.Fatalf("expected failure, out=%q", out)
	}
	for _, want := range []string{"cannot enter " + confDir, "setfacl -m u:statuspage-agent:x " + confDir, "not touched"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "chgrp") {
		t.Errorf("blamed the file for a directory problem: %q", out)
	}
}

func TestInstallerPreflightUsesTheEffectiveServiceAccount(t *testing.T) {
	h := newInstallHarness(t)
	h.systemd("svc2", "svcgrp", h.conf)
	h.script("id", `if [ "$1" = "-g" ]; then echo 1000; exit 0; fi
[ "$1" = "svc2" ]`)
	h.denied = []string{h.conf}
	code, out := h.run("upgrade")
	if code == 0 || !strings.Contains(out, "'svc2'") || !strings.Contains(out, "chgrp svcgrp") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

// The preflight fails closed: every way of not being able to prove access is an error that
// leaves the installed binary, config and service alone.
func TestInstallerPreflightFailsClosedWhenItCannotProveAccess(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *installHarness)
		want  string
	}{
		{"no identity tool", func(h *installHarness) { h.runuser = "exit 125" }, "no working runuser or setpriv"},
		{"switching identity fails", func(h *installHarness) { h.runuser = "exit 126" }, "switching to that account failed (exit 126)"},
		{"systemd cannot be asked", func(h *installHarness) { h.script("systemctl", "exit 1") }, "cannot ask systemd"},
		{"unit runs the service as root", func(h *installHarness) { h.systemd("", "", h.conf) }, "would run as root"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newInstallHarness(t)
			c.setup(h)
			before := h.tree()
			code, out := h.run("upgrade")
			if code == 0 || strings.Contains(out, "preflight-ok") {
				t.Fatalf("continued although access was not proven: code=%d out=%q", code, out)
			}
			if !strings.Contains(out, c.want) || !strings.Contains(out, "not touched") {
				t.Errorf("output lacks %q or the not-touched statement: %q", c.want, out)
			}
			if h.tree() != before {
				t.Error("a failed preflight changed the tree")
			}
		})
	}
}

// The first install has no unit to query yet: it checks the account the unit will name.
func TestInstallerPreflightOnFreshInstallDoesNotNeedASystemdUnit(t *testing.T) {
	h := newInstallHarness(t)
	h.script("systemctl", "exit 1")
	code, out := h.run("install")
	if code != 0 || !strings.Contains(out, "preflight-ok") {
		t.Errorf("code=%d out=%q", code, out)
	}
	h.denied = []string{h.conf}
	code, out = h.run("install")
	if code == 0 || !strings.Contains(out, "The service was not started") {
		t.Errorf("a fresh install must still fail on an unreadable config: code=%d out=%q", code, out)
	}
}

// The config checked is the one systemd will start the service with, drop-ins included.
func TestInstallerPreflightChecksTheConfigSystemdActuallyUses(t *testing.T) {
	h := newInstallHarness(t)
	other := filepath.Join(filepath.Dir(h.conf), "other.conf")
	h.write(other, "x")
	h.systemd("statuspage-agent", "statuspage-agent", other)
	h.denied = []string{other}
	code, out := h.run("upgrade")
	if code == 0 || !strings.Contains(out, other) {
		t.Errorf("should have refused %s: code=%d out=%q", other, code, out)
	}
}

// Ordering is the point of the feature: the check has to come before anything is replaced.
func TestInstallerRunsPreflightBeforeAnyChange(t *testing.T) {
	src := installSource(t)
	at := func(needle string) int {
		t.Helper()
		i := strings.Index(src, needle)
		if i < 0 {
			t.Fatalf("install.sh no longer contains %q", needle)
		}
		return i
	}

	upgrade := at("preflight_config_access upgrade")
	for _, later := range []string{
		`curl -fsSL "${DOWNLOAD_URL}"`,
		`install -m 0755 "${TMP_DIR}/serveragent" "${STAGED}"`,
		`mv -f "${STAGED}"`,
		`systemctl restart statuspage-serveragent.service`,
	} {
		if at(later) < upgrade {
			t.Errorf("upgrade preflight runs after %q", later)
		}
	}

	install := at("preflight_config_access install")
	if install < at(`chmod 0640 "${CONFIG_FILE}"`) {
		t.Error("the install-time preflight runs before the config is written")
	}
	if install > at("systemctl daemon-reload\nsystemctl enable") {
		t.Error("the install-time preflight runs after the service is enabled")
	}
}

// A fresh install must not depend on root's umask, and must never loosen a directory that
// already exists.
func TestInstallerCreatesConfigDirectoryWithExplicitModeOnlyIfAbsent(t *testing.T) {
	src := installSource(t)
	want := `[ -d "${CONFIG_DIR}" ] || install -d -m 0755 -o root -g root "${CONFIG_DIR}"`
	if !strings.Contains(src, want) {
		t.Errorf("install.sh no longer contains %q", want)
	}
	if strings.Contains(src, `mkdir -p "${CONFIG_DIR}"`) {
		t.Error("install.sh creates the config directory with the caller's umask again")
	}
}
