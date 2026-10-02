package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The permission scenarios below run against a real temporary tree whose modes are set
// with chmod, but the "service user" is a different, fake identity and the access decision
// is made by an evaluator that implements the ordinary owner/group/other rules. That keeps
// the tests deterministic and independent of who runs them: as root every chmod-based test
// would otherwise pass vacuously.

const fakeServiceUID = 4242

// execStartFor renders what `systemctl show -p ExecStart --value` prints for a unit that
// starts the agent with the given config path.
func execStartFor(config string) string {
	return "{ path=/usr/local/bin/serveragent ; argv[]=/usr/local/bin/serveragent -config " + config +
		" ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
}

// svcIdentity is the fake service account: its gid is either the owning group of the files
// the test creates (so the service group "matches") or an unrelated one.
type svcIdentity struct {
	uid    uint32
	groups map[uint32]bool
}

// mayAccess implements classic UNIX permission semantics for want = 4 (read) or 1 (search).
func (s svcIdentity) mayAccess(fi fs.FileInfo, want fs.FileMode) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	perm := fi.Mode().Perm()
	switch {
	case st.Uid == s.uid:
		return perm>>6&want != 0
	case s.groups[st.Gid]:
		return perm>>3&want != 0
	default:
		return perm&want != 0
	}
}

type permFixture struct {
	t       *testing.T
	root    string // stands in for /etc
	dir     string // stands in for /etc/statuspage
	conf    string
	unit    string // marks the supported systemd installation; its contents are not read
	show    map[string]string
	showErr error
	errOut  *bytes.Buffer
	svc     svcIdentity
	check   *serviceConfigCheck
}

// newPermFixture builds root/statuspage/serveragent.conf (0755 / 0640 by default) and a
// service identity whose group matches the files' group.
func newPermFixture(t *testing.T) *permFixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "statuspage")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "serveragent.conf")
	if err := os.WriteFile(conf, []byte("SP_ENDPOINT=https://example.invalid\nSP_INGEST_KEY=secret-value\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(root, "unit.service")
	if err := os.WriteFile(unit, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(conf)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)

	f := &permFixture{t: t, root: root, dir: dir, conf: conf, unit: unit, errOut: &bytes.Buffer{}}
	f.show = map[string]string{
		"User":      "statuspage-agent",
		"Group":     "statuspage-agent",
		"ExecStart": execStartFor(conf),
	}
	f.svc = svcIdentity{uid: fakeServiceUID, groups: map[uint32]bool{st.Gid: true}}
	f.check = &serviceConfigCheck{
		UnitPath:   unit,
		ConfigPath: "/ignored/because/systemd/names/one",
		ErrOut:     f.errOut,
		Stat:       os.Stat,
		Show: func(_ context.Context, property string) (string, error) {
			if f.showErr != nil {
				return "", f.showErr
			}
			return f.show[property], nil
		},
		LookupUser: func(name string) (*user.User, error) {
			if name != "statuspage-agent" {
				return nil, user.UnknownUserError(name)
			}
			return &user.User{Uid: strconv.Itoa(fakeServiceUID), Gid: "1", Username: name}, nil
		},
		Access: func(_ context.Context, _ *user.User, flag, path string) (bool, error) {
			if !strings.HasPrefix(path, f.root) {
				return true, nil // above the sandbox (the host's own temp dirs): not under test
			}
			fi, err := os.Stat(path)
			if err != nil {
				return false, nil
			}
			want := fs.FileMode(4)
			if flag == "-x" {
				want = 1
			}
			// Searching a directory also requires every directory above it to be searchable,
			// which is exactly what the real kernel check does.
			for p := filepath.Dir(path); strings.HasPrefix(p, f.root) && p != filepath.Dir(f.root); p = filepath.Dir(p) {
				pi, err := os.Stat(p)
				if err != nil || !f.svc.mayAccess(pi, 1) {
					return false, nil
				}
			}
			return f.svc.mayAccess(fi, want), nil
		},
	}
	return f
}

// snapshot records every path under root with its mode and contents, so a test can prove a
// failed preflight changed nothing.
func (f *permFixture) snapshot() string {
	f.t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %v", strings.TrimPrefix(p, f.root), info.Mode())
		if !d.IsDir() {
			body, _ := os.ReadFile(p)
			fmt.Fprintf(&b, " %q", body)
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return b.String()
}

func (f *permFixture) wrongGroup() { f.svc.groups = map[uint32]bool{999999: true} }

func TestConfigCheckAcceptsCanonicalLayout(t *testing.T) {
	f := newPermFixture(t)
	before := f.snapshot()
	if err := f.check.Run(context.Background()); err != nil {
		t.Fatalf("canonical layout rejected: %v", err)
	}
	if f.errOut.Len() != 0 {
		t.Errorf("unexpected output: %q", f.errOut.String())
	}
	if f.snapshot() != before {
		t.Error("the check modified the tree")
	}
}

func TestConfigCheckMissingConfig(t *testing.T) {
	f := newPermFixture(t)
	if err := os.Remove(f.conf); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot()
	err := f.check.Run(context.Background())
	wantCode(t, err, exitConfig)
	if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("error = %v", err)
	}
	if f.snapshot() != before {
		t.Error("the check modified the tree")
	}
}

// Readable by root, unreadable by the service user: the exact failure from the rollout,
// minus the directory.
func TestConfigCheckFileNotReadableByServiceUser(t *testing.T) {
	f := newPermFixture(t)
	if err := os.Chmod(f.conf, 0o600); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot()
	err := f.check.Run(context.Background())
	wantCode(t, err, exitConfig)
	for _, want := range []string{"statuspage-agent", f.conf, "chgrp statuspage-agent", "chmod 0640", "nothing was changed", "0600"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Error("the error leaked config contents")
	}
	if f.snapshot() != before {
		t.Error("a failed check modified the tree")
	}
}

func TestConfigCheckWrongGroup(t *testing.T) {
	f := newPermFixture(t)
	f.wrongGroup() // file is 0640 but its group is not the service's
	before := f.snapshot()
	err := f.check.Run(context.Background())
	wantCode(t, err, exitConfig)
	if !strings.Contains(err.Error(), "chgrp statuspage-agent") {
		t.Errorf("error = %v", err)
	}
	if f.snapshot() != before {
		t.Error("a failed check modified the tree")
	}
}

// The v0.1.4 rollout failure: the config file is fine, the directory above it is 0700.
func TestConfigCheckParentDirectoryNotSearchable(t *testing.T) {
	f := newPermFixture(t)
	if err := os.Chmod(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Make the service user a non-owner so 0700 denies it (the test user owns the tree).
	before := f.snapshot()
	err := f.check.Run(context.Background())
	wantCode(t, err, exitConfig)
	for _, want := range []string{"cannot enter " + f.dir, "setfacl -m u:statuspage-agent:x " + f.dir, "traversal only", "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if f.snapshot() != before {
		t.Error("a failed check modified the tree")
	}
	// The error must point at the directory, not suggest loosening the file.
	if strings.Contains(err.Error(), "chgrp") {
		t.Errorf("blamed the file for a directory problem: %v", err)
	}
}

func TestConfigCheckReportsOutermostBlockedDirectory(t *testing.T) {
	f := newPermFixture(t)
	inner := filepath.Join(f.dir, "inner")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(inner, "serveragent.conf")
	if err := os.WriteFile(conf, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.show["ExecStart"] = execStartFor(conf)
	if err := os.Chmod(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	err := f.check.Run(context.Background())
	wantCode(t, err, exitConfig)
	if !strings.Contains(err.Error(), "cannot enter "+f.dir) {
		t.Errorf("should name the outermost blocked directory %s: %v", f.dir, err)
	}
}

func TestConfigCheckUsesConfigPathFromSystemd(t *testing.T) {
	f := newPermFixture(t)
	other := filepath.Join(f.dir, "other.conf")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil { // unreadable
		t.Fatal(err)
	}
	f.show["ExecStart"] = execStartFor(other)
	// ConfigPath (the flag value) points at a good file; systemd's effective path must win.
	f.check.ConfigPath = f.conf
	err := f.check.Run(context.Background())
	wantCode(t, err, exitConfig)
	if !strings.Contains(err.Error(), other) {
		t.Errorf("checked the wrong path: %v", err)
	}
}

func TestConfigCheckFallsBackToFlagPathWhenSystemdNamesNone(t *testing.T) {
	f := newPermFixture(t)
	f.show["ExecStart"] = "{ path=/usr/local/bin/serveragent ; argv[]=/usr/local/bin/serveragent ; ignore_errors=no }"
	f.check.ConfigPath = f.conf
	if err := f.check.Run(context.Background()); err != nil {
		t.Fatalf("flag path should be used and is readable: %v", err)
	}
}

// The one deliberate exception to failing closed: no systemd unit at all means this is not
// the supported installation, so there is no service account to check. It must say so.
func TestConfigCheckStandaloneBinaryWithoutUnitIsSkippedExplicitly(t *testing.T) {
	f := newPermFixture(t)
	if err := os.Remove(f.unit); err != nil {
		t.Fatal(err)
	}
	f.showErr = errors.New("must not be consulted")
	f.chmodConfigUnreadable()
	if err := f.check.Run(context.Background()); err != nil {
		t.Fatalf("a standalone binary must not be blocked: %v", err)
	}
	if !strings.Contains(f.errOut.String(), "standalone") {
		t.Errorf("expected an explicit standalone note, got %q", f.errOut.String())
	}
}

func TestConfigCheckUnitPresentButUninspectableFailsClosed(t *testing.T) {
	f := newPermFixture(t)
	f.check.Stat = func(path string) (fs.FileInfo, error) {
		if path == f.unit {
			return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
		}
		return os.Stat(path)
	}
	wantCode(t, f.check.Run(context.Background()), exitGeneric)
}

func TestConfigCheckUnknownServiceUserIsRejected(t *testing.T) {
	f := newPermFixture(t)
	f.show["User"] = "nobody-such-user"
	wantCode(t, f.check.Run(context.Background()), exitConfig)
}

func TestConfigCheckFailsClosedWhenSystemdCannotBeAsked(t *testing.T) {
	f := newPermFixture(t)
	f.showErr = errors.New("Failed to connect to bus")
	f.chmodConfigUnreadable() // would be caught, but the identity is unknown first
	err := f.check.Run(context.Background())
	wantCode(t, err, exitGeneric)
	if !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("error = %v", err)
	}
}

func TestConfigCheckFailsClosedWhenTheUnitRunsAsRoot(t *testing.T) {
	f := newPermFixture(t)
	f.show["User"] = ""
	err := f.check.Run(context.Background())
	wantCode(t, err, exitConfig)
	if !strings.Contains(err.Error(), "root") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("error = %v", err)
	}
}

func TestConfigCheckFailsClosedWithoutRoot(t *testing.T) {
	f := newPermFixture(t)
	f.check.Access = func(context.Context, *user.User, string, string) (bool, error) { return false, errNotRoot }
	err := f.check.Run(context.Background())
	wantCode(t, err, exitPermission)
	if !strings.Contains(err.Error(), "sudo") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("error = %v", err)
	}
}

func TestConfigCheckUnverifiableIsAnErrorNotAPass(t *testing.T) {
	f := newPermFixture(t)
	f.check.Access = func(context.Context, *user.User, string, string) (bool, error) {
		return false, fmt.Errorf("%w: no test binary", errAccessUnverifiable)
	}
	err := f.check.Run(context.Background())
	wantCode(t, err, exitGeneric)
	if !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("error = %v", err)
	}
}

func TestConfigCheckUnexpectedAccessErrorFails(t *testing.T) {
	f := newPermFixture(t)
	f.check.Access = func(context.Context, *user.User, string, string) (bool, error) {
		return false, errors.New("boom")
	}
	wantCode(t, f.check.Run(context.Background()), exitGeneric)
}

func (f *permFixture) chmodConfigUnreadable() {
	f.t.Helper()
	if err := os.Chmod(f.conf, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func TestConfigFromExecStart(t *testing.T) {
	cases := []struct{ name, value, want string }{
		{"canonical", execStartFor("/etc/statuspage/serveragent.conf"), "/etc/statuspage/serveragent.conf"},
		{"double dash and equals", "{ path=/x ; argv[]=/x --config=/etc/a.conf ; ignore_errors=no }", "/etc/a.conf"},
		{"single dash equals", "{ path=/x ; argv[]=/x -config=/etc/b.conf -other ; ignore_errors=no }", "/etc/b.conf"},
		{"no config argument", "{ path=/x ; argv[]=/x ; ignore_errors=no }", ""},
		{"no argv at all", "{ path=/x }", ""},
		{"first of several commands", "{ path=/x ; argv[]=/x -config /one ; ignore_errors=no }\n{ path=/y ; argv[]=/y -config /two ; ignore_errors=no }", "/one"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := configFromExecStart(c.value); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// --- Updater integration ----------------------------------------------------------------

func TestUpdateConfigCheckFailureStopsBeforeAnyDownload(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	called := 0
	f.u.ConfigCheck = func(context.Context) error {
		called++
		return fail(exitConfig, "the service user cannot read the config")
	}
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitConfig)
	if called != 1 {
		t.Errorf("config check ran %d times, want 1", called)
	}
	f.assertUntouched(t)
	f.rel.mu.Lock()
	defer f.rel.mu.Unlock()
	for path, n := range f.rel.hits {
		if strings.HasPrefix(path, "/dist/serveragent/") {
			t.Errorf("fetched %s (%d) after the preflight failed", path, n)
		}
	}
}

func TestUpdateConfigCheckPassesThenInstalls(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	called := 0
	f.u.ConfigCheck = func(context.Context) error { called++; return nil }
	res, err := f.u.Run(context.Background())
	if err != nil || res.UpToDate || called != 1 {
		t.Fatalf("res=%+v err=%v called=%d", res, err, called)
	}
	if got, _ := os.ReadFile(f.exe); string(got) == oldBinary {
		t.Error("binary was not replaced after a passing preflight")
	}
}

func TestUpdateConfigCheckSkippedWhenNothingToInstall(t *testing.T) {
	for _, current := range []string{"0.1.4", "0.2.0"} { // up to date, and newer than advertised
		f := newFixture(t, current, "0.1.4", true)
		f.u.ConfigCheck = func(context.Context) error {
			t.Errorf("config check ran although %s needs no update", current)
			return nil
		}
		res, err := f.u.Run(context.Background())
		if err != nil || !res.UpToDate {
			t.Fatalf("%s: res=%+v err=%v", current, res, err)
		}
	}
}

func TestRunUpdateWiresTheServiceConfigCheck(t *testing.T) {
	// runUpdateWith must install the real check; a tweak that replaces it proves it was set.
	var wired bool
	conf := filepath.Join(t.TempDir(), "serveragent.conf")
	if err := os.WriteFile(conf, []byte("SP_ENDPOINT=https://example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	runUpdateWith(conf, &out, &errOut, func(u *Updater) {
		wired = u.ConfigCheck != nil
		u.ConfigCheck = nil
		u.GOOS, u.GOARCH = "plan9", "none" // stop right after, without any network
	})
	if !wired {
		t.Error("runUpdateWith did not set ConfigCheck")
	}
}

// Real identity switch: only meaningful (and only possible) as root.
func TestAccessAsUserDeniesAnUnprivilegedAccount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching identity needs root")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("no nobody account")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "serveragent.conf")
	if err := os.WriteFile(conf, []byte("x"), 0o600); err != nil { // root-only
		t.Fatal(err)
	}
	ok, err := accessAsUser(context.Background(), nobody, "-r", conf)
	if err != nil || ok {
		t.Fatalf("root-only file readable by nobody? ok=%v err=%v", ok, err)
	}
	if err := os.Chmod(conf, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := accessAsUser(context.Background(), nobody, "-r", conf); err != nil || !ok {
		t.Fatalf("world-readable file unreadable by nobody: ok=%v err=%v", ok, err)
	}
}

func TestAccessAsUserReportsNotRootWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	_, err := accessAsUser(context.Background(), &user.User{Uid: "1", Gid: "1"}, "-r", "/etc/passwd")
	if !errors.Is(err, errNotRoot) {
		t.Errorf("err = %v, want errNotRoot", err)
	}
}
