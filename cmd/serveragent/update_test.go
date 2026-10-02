package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// None of these tests may touch the real test binary: every Updater is built with an
// injected Executable pointing into t.TempDir().

const fakeArch = "amd64"

// release is a fake distribution endpoint: the version API plus /dist/serveragent/.
type release struct {
	mu sync.Mutex

	version       string
	versionStatus int
	versionBody   string // overrides the JSON body when non-empty

	files  map[string][]byte // served under /dist/serveragent/<name>
	status map[string]int    // per-file status overrides
	hits   map[string]int    // path -> request count
}

func newRelease(version string) *release {
	return &release{
		version: version,
		files:   map[string][]byte{},
		status:  map[string]int{},
		hits:    map[string]int{},
	}
}

func (r *release) asset(version string) string {
	return fmt.Sprintf("serveragent-%s-linux-%s", version, fakeArch)
}

// publish adds an artifact and a matching SHA256SUMS entry.
func (r *release) publish(version string, body []byte) {
	sum := sha256.Sum256(body)
	r.files[r.asset(version)] = body
	line := hex.EncodeToString(sum[:]) + "  " + r.asset(version) + "\n"
	r.files["SHA256SUMS"] = append(r.files["SHA256SUMS"], line...)
}

func (r *release) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits[req.URL.Path]++
		r.mu.Unlock()

		if req.URL.Path == "/api/server-agent/version" {
			if r.versionStatus != 0 {
				w.WriteHeader(r.versionStatus)
				return
			}
			if r.versionBody != "" {
				io.WriteString(w, r.versionBody)
				return
			}
			fmt.Fprintf(w, `{"version":%q}`, r.version)
			return
		}
		name := strings.TrimPrefix(req.URL.Path, "/dist/serveragent/")
		if code, ok := r.status[name]; ok {
			w.WriteHeader(code)
			return
		}
		body, ok := r.files[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(body)
	})
}

func (r *release) hitCount(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[path]
}

// totalHits counts every request the fake endpoint received.
func (r *release) totalHits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.hits {
		n += c
	}
	return n
}

// fixture bundles a TLS endpoint and an Updater aimed at a fake install directory.
type fixture struct {
	rel *release
	srv *httptest.Server
	u   *Updater
	exe string
	dir string
	out *bytes.Buffer
	err *bytes.Buffer
}

const oldBinary = "old-binary-contents"

func newFixture(t *testing.T, current, latest string, publishLatest bool) *fixture {
	t.Helper()
	rel := newRelease(latest)
	if publishLatest {
		rel.publish(latest, []byte("new-binary-contents-"+latest))
	}
	srv := httptest.NewTLSServer(rel.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	exe := filepath.Join(dir, "serveragent")
	if err := os.WriteFile(exe, []byte(oldBinary), 0o750); err != nil {
		t.Fatal(err)
	}
	f := &fixture{rel: rel, srv: srv, exe: exe, dir: dir, out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	f.u = &Updater{
		Config:         &Config{Endpoint: srv.URL},
		CurrentVersion: current,
		GOOS:           "linux",
		GOARCH:         fakeArch,
		HTTP:           srv.Client(),
		Out:            f.out,
		ErrOut:         f.err,
		Executable:     func() (string, error) { return exe, nil },
		CreateTemp:     os.CreateTemp,
		Rename:         os.Rename,
		SmokeTest: func(_ context.Context, _ string) (string, error) {
			return fmt.Sprintf("statuspage-serveragent %s (linux/%s)\n", latest, fakeArch), nil
		},
	}
	return f
}

// assertUntouched checks the installed binary is byte-identical and no staging file is
// left behind.
func (f *fixture) assertUntouched(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(f.exe)
	if err != nil || string(got) != oldBinary {
		t.Fatalf("installed binary changed: %q, %v", got, err)
	}
	f.assertNoTemp(t)
}

func (f *fixture) assertNoTemp(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(f.dir)
	for _, e := range entries {
		if e.Name() != "serveragent" {
			t.Fatalf("leftover file in install dir: %s", e.Name())
		}
	}
}

func wantCode(t *testing.T, err error, code int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected exit code %d, got success", code)
	}
	if got := updateCode(err); got != code {
		t.Fatalf("exit code = %d (%v), want %d", got, err, code)
	}
}

func TestParseAgentVersion(t *testing.T) {
	valid := map[string]agentVersion{
		"0.1.3":    {0, 1, 3},
		"v0.1.3":   {0, 1, 3},
		" 1.20.3 ": {1, 20, 3},
		"0.1.10":   {0, 1, 10},
		"10.0.0":   {10, 0, 0},
	}
	for in, want := range valid {
		got, err := parseAgentVersion(in)
		if err != nil || got != want {
			t.Errorf("parseAgentVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "v", "1", "1.2", "1.2.3.4", "1.2.x", "1.2.3-rc1", "1.2.3+build", "-1.2.3", "1..3", "dev", "1.2.+3"} {
		if _, err := parseAgentVersion(in); err == nil {
			t.Errorf("parseAgentVersion(%q) should fail", in)
		}
	}
}

func TestCompareAgentVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.9", "0.1.10", -1}, // numeric, not lexical
		{"0.1.10", "0.1.9", 1},
		{"0.1.3", "0.1.3", 0},
		{"0.2.0", "0.1.99", 1},
		{"1.0.0", "0.99.99", 1},
		{"v0.1.3", "0.1.3", 0},
	}
	for _, c := range cases {
		a, _ := parseAgentVersion(c.a)
		b, _ := parseAgentVersion(c.b)
		if got := compareAgentVersions(a, b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	h1 := strings.Repeat("a", 64)
	h2 := strings.Repeat("b", 64)
	asset := "serveragent-0.1.3-linux-amd64"

	got, err := checksumFor([]byte(h1+"  other\n"+strings.ToUpper(h2)+" *"+asset+"\n"), asset)
	if err != nil || got != h2 {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := checksumFor([]byte(h1+"  other\n"), asset); err == nil {
		t.Error("missing entry must fail")
	}
	if _, err := checksumFor([]byte("nothex  "+asset+"\n"), asset); err == nil {
		t.Error("malformed hash must fail")
	}
	if _, err := checksumFor([]byte(h1+"  "+asset+"\n"+h2+"  "+asset+"\n"), asset); err == nil {
		t.Error("conflicting entries must fail")
	}
	if got, err := checksumFor([]byte(h1+"  "+asset+"\n"+h1+"  "+asset+"\n"), asset); err != nil || got != h1 {
		t.Errorf("identical duplicates should pass: %q, %v", got, err)
	}
	// A prefix of the asset name must not match.
	if _, err := checksumFor([]byte(h1+"  "+asset+".sig\n"), asset); err == nil {
		t.Error("partial name match must not count")
	}
}

func TestUpdateAlreadyCurrent(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.3", true)
	res, err := f.u.Run(context.Background())
	if err != nil || !res.UpToDate || res.To != "0.1.3" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if n := f.rel.hitCount("/dist/serveragent/" + f.rel.asset("0.1.3")); n != 0 {
		t.Errorf("downloaded the artifact although already current (%d)", n)
	}
	f.assertUntouched(t)
}

func TestUpdateDoesNotDowngrade(t *testing.T) {
	f := newFixture(t, "0.1.5", "0.1.3", true)
	res, err := f.u.Run(context.Background())
	if err != nil || !res.UpToDate {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	f.assertUntouched(t)
}

func TestUpdateInstallsNewerRelease(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)

	// A handle opened before the swap must keep seeing the old inode: the running
	// process is unaffected by an atomic rename.
	old, err := os.Open(f.exe)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	res, err := f.u.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.UpToDate || res.From != "0.1.3" || res.To != "0.1.4" || res.Path != f.exe {
		t.Fatalf("unexpected result %+v", res)
	}
	got, _ := os.ReadFile(f.exe)
	if string(got) != "new-binary-contents-0.1.4" {
		t.Fatalf("binary not replaced: %q", got)
	}
	st, _ := os.Stat(f.exe)
	if st.Mode().Perm() != 0o750 {
		t.Errorf("mode = %v, want the previous 0750 preserved", st.Mode().Perm())
	}
	if prev, _ := io.ReadAll(old); string(prev) != oldBinary {
		t.Errorf("pre-swap handle saw %q; the old inode must be unaffected", prev)
	}
	f.assertNoTemp(t)
}

func TestUpdateDefaultsToExecutableModeWhenOldHasNone(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	if err := os.Chmod(f.exe, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(f.exe)
	if st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", st.Mode().Perm())
	}
}

func TestUpdateVersionLookupFailures(t *testing.T) {
	cases := map[string]func(*release){
		"http 500":       func(r *release) { r.versionStatus = http.StatusInternalServerError },
		"garbage json":   func(r *release) { r.versionBody = "not json" },
		"empty version":  func(r *release) { r.versionBody = `{"version":""}` },
		"junk version":   func(r *release) { r.versionBody = `{"version":"latest"}` },
		"prerelease tag": func(r *release) { r.versionBody = `{"version":"0.2.0-rc1"}` },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "0.1.3", "0.1.4", true)
			mutate(f.rel)
			_, err := f.u.Run(context.Background())
			wantCode(t, err, exitRelease)
			f.assertUntouched(t)
		})
	}
}

func TestUpdateUnreachableEndpoint(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	f.srv.Close()
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitRelease)
	f.assertUntouched(t)
}

func TestUpdateMissingArtifactIsReleaseError(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	delete(f.rel.files, f.rel.asset("0.1.4")) // SHA256SUMS still lists it
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitRelease)
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say the artifact was not found: %v", err)
	}
	f.assertUntouched(t)
}

func TestUpdateMissingChecksumFileIsReleaseError(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	delete(f.rel.files, "SHA256SUMS")
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitRelease)
	if n := f.rel.hitCount("/dist/serveragent/" + f.rel.asset("0.1.4")); n != 0 {
		t.Errorf("artifact fetched without a checksum file (%d)", n)
	}
	f.assertUntouched(t)
}

func TestUpdateServerErrorOnArtifactIsReleaseError(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	f.rel.status[f.rel.asset("0.1.4")] = http.StatusBadGateway
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitRelease)
	f.assertUntouched(t)
}

func TestUpdateMissingChecksumEntryIsVerifyError(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	f.rel.files["SHA256SUMS"] = []byte(strings.Repeat("c", 64) + "  serveragent-0.1.4-linux-arm64\n")
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitVerify)
	if n := f.rel.hitCount("/dist/serveragent/" + f.rel.asset("0.1.4")); n != 0 {
		t.Errorf("artifact fetched without a checksum entry (%d)", n)
	}
	f.assertUntouched(t)
}

func TestUpdateBadChecksumIsVerifyError(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	f.rel.files[f.rel.asset("0.1.4")] = []byte("tampered")
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitVerify)
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("unexpected message: %v", err)
	}
	f.assertUntouched(t)
}

func TestUpdateSmokeTestMismatchIsVerifyError(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	f.u.SmokeTest = func(context.Context, string) (string, error) {
		return "statuspage-serveragent 0.1.2 (linux/amd64)\n", nil
	}
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitVerify)
	f.assertUntouched(t)
}

func TestUpdateSmokeTestFailureIsVerifyError(t *testing.T) {
	for name, smoke := range map[string]func(context.Context, string) (string, error){
		"exec error":    func(context.Context, string) (string, error) { return "", errors.New("exec format error") },
		"unparseable":   func(context.Context, string) (string, error) { return "hello", nil },
		"empty output":  func(context.Context, string) (string, error) { return "", nil },
		"wrong program": func(context.Context, string) (string, error) { return "other 0.1.4 (linux/amd64)", nil },
		"bad version":   func(context.Context, string) (string, error) { return "statuspage-serveragent dev (linux/amd64)", nil },
		"prefix version": func(context.Context, string) (string, error) {
			return "statuspage-serveragent 0.1.40 (linux/amd64)", nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "0.1.3", "0.1.4", true)
			f.u.SmokeTest = smoke
			_, err := f.u.Run(context.Background())
			wantCode(t, err, exitVerify)
			f.assertUntouched(t)
		})
	}
}

func TestUpdateUnsupportedPlatform(t *testing.T) {
	for _, p := range []struct{ goos, arch string }{
		{"darwin", "arm64"}, {"windows", "amd64"}, {"linux", "386"}, {"linux", "riscv64"},
	} {
		t.Run(p.goos+"/"+p.arch, func(t *testing.T) {
			f := newFixture(t, "0.1.3", "0.1.4", true)
			f.u.GOOS, f.u.GOARCH = p.goos, p.arch
			_, err := f.u.Run(context.Background())
			wantCode(t, err, exitUnsupported)
			if f.rel.totalHits() != 0 {
				t.Error("an unsupported platform must fail before any network request")
			}
			f.assertUntouched(t)
		})
	}
}

func TestUpdateRejectsPlainHTTPEndpoint(t *testing.T) {
	rel := newRelease("0.1.4")
	rel.publish("0.1.4", []byte("x"))
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()

	f := newFixture(t, "0.1.3", "0.1.4", true)
	f.u.Config = &Config{Endpoint: srv.URL}
	f.u.HTTP = srv.Client()

	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitConfig)
	if rel.totalHits() != 0 {
		t.Error("no request may be sent over plain HTTP")
	}

	// The explicit development override is honoured, exactly like the ingest path.
	f.u.Config = &Config{Endpoint: srv.URL, Insecure: true}
	if _, err := f.u.Run(context.Background()); err != nil {
		t.Fatalf("SP_INSECURE should allow http: %v", err)
	}
}

func TestUpdateRejectsRedirectToHTTP(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "should never be fetched")
	}))
	defer plain.Close()

	f := newFixture(t, "0.1.3", "0.1.4", true)
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dist/") {
			http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
			return
		}
		f.rel.handler().ServeHTTP(w, r)
	}))
	defer redirecting.Close()
	f.u.Config = &Config{Endpoint: redirecting.URL}
	f.u.HTTP = newUpdateHTTPClient(false)
	f.u.HTTP.Transport = redirecting.Client().Transport

	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitRelease)
	if !strings.Contains(err.Error(), "non-HTTPS") {
		t.Errorf("expected a redirect refusal, got %v", err)
	}
	f.assertUntouched(t)
}

func TestUpdateOversizeArtifactIsRejected(t *testing.T) {
	old := maxBinaryBytes
	maxBinaryBytes = 16
	defer func() { maxBinaryBytes = old }()

	f := newFixture(t, "0.1.3", "0.1.4", false)
	f.rel.publish("0.1.4", bytes.Repeat([]byte("z"), 64))
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitVerify)
	f.assertUntouched(t)
}

func TestUpdateEmptyArtifactIsRejected(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", false)
	f.rel.publish("0.1.4", []byte{})
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitRelease)
	f.assertUntouched(t)
}

// An unwritable install directory is simulated by an injected failing CreateTemp rather
// than chmod, so the test behaves identically as root and in CI containers.
func TestUpdateUnwritableTargetFailsBeforeDownloading(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	f.u.CreateTemp = func(dir, pattern string) (*os.File, error) {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
	}
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitPermission)
	if !strings.Contains(err.Error(), "sudo") {
		t.Errorf("permission errors should point at sudo: %v", err)
	}
	if n := f.rel.hitCount("/dist/serveragent/" + f.rel.asset("0.1.4")); n != 0 {
		t.Errorf("downloaded the artifact although the target is unwritable (%d)", n)
	}
	f.assertUntouched(t)
}

func TestUpdateRenameFailureLeavesOldBinaryAndNoTemp(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	f.u.Rename = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: fs.ErrPermission}
	}
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitPermission)
	f.assertUntouched(t)
}

func TestUpdateStagingFileLivesBesideTarget(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	var stagedIn string
	f.u.CreateTemp = func(dir, pattern string) (*os.File, error) {
		stagedIn = dir
		return os.CreateTemp(dir, pattern)
	}
	if _, err := f.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stagedIn != f.dir {
		t.Errorf("staged in %q, want the target directory %q so the rename is atomic", stagedIn, f.dir)
	}
}

func TestUpdateWarnsWhenExecutableIsNotTheServicePath(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	if _, err := f.u.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.err.String(), "warning") || !strings.Contains(f.err.String(), installedBinaryPath) {
		t.Errorf("expected a path warning, got %q", f.err.String())
	}
}

func TestUpdateNoWarningForStandardPath(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	// Pretend the running binary is the standard path, but redirect every write into the
	// temp dir so the real /usr/local/bin is never touched.
	f.u.Executable = func() (string, error) { return installedBinaryPath, nil }
	f.u.CreateTemp = func(_, pattern string) (*os.File, error) { return os.CreateTemp(f.dir, pattern) }
	var renamedTo string
	f.u.Rename = func(oldPath, newPath string) error {
		renamedTo = newPath
		return os.Remove(oldPath)
	}
	res, err := f.u.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.err.Len() != 0 {
		t.Errorf("unexpected warning for the standard path: %q", f.err.String())
	}
	if res.Path != installedBinaryPath || renamedTo != installedBinaryPath {
		t.Errorf("result path %q / rename target %q", res.Path, renamedTo)
	}
}

func TestUpdateFailsWhenExecutablePathUnknown(t *testing.T) {
	f := newFixture(t, "0.1.3", "0.1.4", true)
	f.u.Executable = func() (string, error) { return "", errors.New("no /proc") }
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitGeneric)
}

func TestUpdateRejectsUnparseableCurrentVersion(t *testing.T) {
	f := newFixture(t, "dev", "0.1.4", true)
	_, err := f.u.Run(context.Background())
	wantCode(t, err, exitGeneric)
	f.assertUntouched(t)
}

func TestRunSmokeTestExecutesVersionFlag(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake")
	body := "#!/bin/sh\n[ \"$1\" = \"-version\" ] && echo 'statuspage-serveragent 0.1.4 (linux/amd64)'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runSmokeTest(context.Background(), script)
	if err != nil {
		t.Fatalf("runSmokeTest: %v", err)
	}
	if v, ok := parseVersionOutput(out); !ok || v != "0.1.4" {
		t.Errorf("parsed %q, %v from %q", v, ok, out)
	}
	if _, err := runSmokeTest(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing binary must error")
	}
}

func TestParseVersionOutputMatchesRealVersionLine(t *testing.T) {
	line := fmt.Sprintf("statuspage-serveragent %s (%s/%s)\n", "0.1.3", "linux", "arm64")
	if v, ok := parseVersionOutput(line); !ok || v != "0.1.3" {
		t.Errorf("got %q, %v", v, ok)
	}
}

// runUpdate builds an Updater around the real os.Executable(), so these tests go through
// runUpdateWith and redirect the executable (and pin the platform) to a temp file.
func sandboxedUpdater(t *testing.T) func(*Updater) {
	exe := filepath.Join(t.TempDir(), "serveragent")
	if err := os.WriteFile(exe, []byte(oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	return func(u *Updater) {
		u.GOOS, u.GOARCH = "linux", fakeArch
		u.Executable = func() (string, error) { return exe, nil }
		u.ConfigCheck = nil // the real check reads this host's systemd unit
	}
}

func TestRunUpdateConfigErrorsExitWithConfigCode(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "serveragent.conf")
	t.Setenv("SP_ENDPOINT", "")
	if err := os.WriteFile(conf, []byte("SP_INSECURE=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runUpdateWith(conf, &out, &errOut, sandboxedUpdater(t)); code != exitConfig {
		t.Fatalf("code = %d (%s), want %d", code, errOut.String(), exitConfig)
	}

	if err := os.WriteFile(conf, []byte("not a key value line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runUpdateWith(conf, &out, &errOut, sandboxedUpdater(t)); code != exitConfig {
		t.Fatalf("malformed config: code = %d, want %d", code, exitConfig)
	}
}

func TestRunUpdateReportsUpToDate(t *testing.T) {
	rel := newRelease("9.9.9")
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()

	old := AgentVersion
	AgentVersion = "9.9.9"
	defer func() { AgentVersion = old }()

	conf := filepath.Join(t.TempDir(), "serveragent.conf")
	body := "SP_ENDPOINT=" + srv.URL + "\nSP_INSECURE=1\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runUpdateWith(conf, &out, &errOut, sandboxedUpdater(t)); code != exitOK {
		t.Fatalf("code = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "already up to date (v9.9.9)") {
		t.Errorf("output = %q", out.String())
	}
}

func TestUpdateCodeDefaultsToGeneric(t *testing.T) {
	if updateCode(nil) != exitOK || updateCode(errors.New("x")) != exitGeneric {
		t.Error("unexpected default exit codes")
	}
}

func TestUpdateAvailable(t *testing.T) {
	for _, tt := range []struct {
		running, advertised string
		want                bool
	}{
		{"0.1.4", "0.1.4", false},
		{"0.1.4", "0.1.5", true},
		{"0.1.9", "0.1.10", true},
		{"0.1.10", "0.1.9", false}, // running newer than advertised: no nag
		{"v0.1.4", "0.1.5", true},
		{"0.1.4", "v0.1.5", true},
		{"v0.1.5", "v0.1.5", false},
		{"0.2.0", "0.1.99", false},
	} {
		got, err := updateAvailable(tt.running, tt.advertised)
		if err != nil || got != tt.want {
			t.Errorf("updateAvailable(%q, %q) = %v, %v; want %v, nil", tt.running, tt.advertised, got, err, tt.want)
		}
	}
	for _, bad := range [][2]string{{"0.1.4", "latest"}, {"dev", "0.1.5"}, {"0.1.4", "0.1"}, {"0.1.4", "0.1.5-rc1"}, {"", "0.1.5"}, {"0.1.4", ""}} {
		if got, err := updateAvailable(bad[0], bad[1]); err == nil || got {
			t.Errorf("updateAvailable(%q, %q) = %v, %v; want an error and false", bad[0], bad[1], got, err)
		}
	}
}

func TestVersionCheckMessage(t *testing.T) {
	if msg := versionCheckMessage("0.1.4", "0.1.5"); !strings.Contains(msg, "a newer agent is available") || !strings.Contains(msg, "sudo "+installedBinaryPath+" --update") {
		t.Errorf("update message = %q", msg)
	}
	for _, tt := range [][2]string{{"0.1.4", "0.1.4"}, {"0.1.5", "0.1.4"}, {"0.1.10", "0.1.9"}, {"v0.1.4", "0.1.4"}, {"0.1.4", ""}} {
		if msg := versionCheckMessage(tt[0], tt[1]); msg != "" {
			t.Errorf("versionCheckMessage(%q, %q) = %q, want silence", tt[0], tt[1], msg)
		}
	}
	msg := versionCheckMessage("0.1.4", "latest")
	if !strings.Contains(msg, "cannot compare agent versions") || strings.Contains(msg, "newer agent is available") {
		t.Errorf("malformed advertised version message = %q", msg)
	}
}

// checkVersion end to end: the daily check logs exactly one actionable line for a strictly
// newer release and stays silent for equal or newer-than-advertised agents.
func TestCheckVersionLogsOnlyForNewerRelease(t *testing.T) {
	advertised := "0.1.5"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q}`, advertised)
	}))
	defer srv.Close()
	agent := &Agent{client: NewClient(&Config{Endpoint: srv.URL, ServerID: "s", IngestKey: "k"})}

	prevVersion, prevOut, prevFlags := AgentVersion, log.Writer(), log.Flags()
	defer func() { AgentVersion = prevVersion; log.SetOutput(prevOut); log.SetFlags(prevFlags) }()
	log.SetFlags(0)

	run := func(running, adv string) string {
		var buf bytes.Buffer
		log.SetOutput(&buf)
		AgentVersion, advertised = running, adv
		agent.checkVersion(context.Background())
		return buf.String()
	}
	if got := run("0.1.4", "0.1.5"); !strings.Contains(got, "a newer agent is available (running 0.1.4, current 0.1.5)") {
		t.Errorf("0.1.4 vs 0.1.5 logged %q", got)
	}
	for _, c := range [][2]string{{"0.1.5", "0.1.5"}, {"0.1.10", "0.1.9"}, {"v0.1.5", "0.1.5"}} {
		if got := run(c[0], c[1]); got != "" {
			t.Errorf("running %s advertised %s logged %q, want silence", c[0], c[1], got)
		}
	}
}
