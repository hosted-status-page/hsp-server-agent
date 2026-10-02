package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Exit codes for `serveragent --update`. They are part of the command's contract, so
// scripts can tell "nothing to do" apart from "could not reach the release" apart from
// "the download did not verify".
const (
	exitOK          = 0
	exitGeneric     = 1 // unexpected local failure
	exitConfig      = 2 // bad flags or configuration (including an unreadable config file)
	exitRelease     = 3 // version lookup, SHA256SUMS or artifact unavailable (network, 404, 5xx)
	exitVerify      = 4 // checksum entry missing, checksum mismatch, or smoke-test mismatch
	exitPermission  = 5 // target directory or binary cannot be written
	exitUnsupported = 6 // OS/architecture without a published artifact
)

const (
	// installedBinaryPath is where install.sh puts the binary and where the systemd unit
	// runs it from.
	installedBinaryPath = "/usr/local/bin/serveragent"

	// serviceName is the systemd unit install.sh creates.
	serviceName = "statuspage-serveragent"

	// distPath is where the release artifacts and SHA256SUMS live, relative to the
	// configured endpoint. It is the same location install.sh downloads from.
	distPath = "/dist/serveragent/"

	// maxChecksumsBytes caps the SHA256SUMS download.
	maxChecksumsBytes = 1 << 20

	updateHTTPTimeout = 5 * time.Minute
	smokeTestTimeout  = 10 * time.Second
)

// maxBinaryBytes caps the artifact download. The real binary is a few MiB; a body beyond
// this is not a release artifact. A variable only so tests can exercise the limit.
var maxBinaryBytes int64 = 64 << 20

// updateError carries the exit code a failed update should produce.
type updateError struct {
	Code int
	Err  error
}

func (e *updateError) Error() string { return e.Err.Error() }
func (e *updateError) Unwrap() error { return e.Err }

func fail(code int, format string, args ...any) *updateError {
	return &updateError{Code: code, Err: fmt.Errorf(format, args...)}
}

// updateCode extracts the exit code from an error returned by Updater.Run.
func updateCode(err error) int {
	if err == nil {
		return exitOK
	}
	var ue *updateError
	if errors.As(err, &ue) {
		return ue.Code
	}
	return exitGeneric
}

// Updater replaces the running agent binary with the release the server advertises.
//
// It deliberately has no knowledge of systemd beyond the message it prints: it never
// restarts the service, and it never executes anything except the freshly downloaded,
// checksum-verified binary with -version, as a smoke test before that binary is put in
// place. The seams below exist so tests can drive every failure path against a temp
// directory without touching the real test binary.
type Updater struct {
	Config         *Config // only Endpoint and Insecure are used
	CurrentVersion string
	GOOS, GOARCH   string
	HTTP           *http.Client
	Out, ErrOut    io.Writer

	// Executable returns the path of the running binary, symlinks resolved.
	Executable func() (string, error)
	// CreateTemp creates the staging file; it must be in dir so the final rename cannot
	// cross filesystems.
	CreateTemp func(dir, pattern string) (*os.File, error)
	// Rename atomically moves the staged file over the installed binary.
	Rename func(oldPath, newPath string) error
	// SmokeTest runs the staged binary with -version and returns its output.
	SmokeTest func(ctx context.Context, binary string) (string, error)
}

// NewUpdater wires an Updater to the real filesystem and the running binary.
func NewUpdater(cfg *Config, out, errOut io.Writer) *Updater {
	u := &Updater{
		Config:         cfg,
		CurrentVersion: AgentVersion,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		Out:            out,
		ErrOut:         errOut,
		Executable:     resolveExecutable,
		CreateTemp:     os.CreateTemp,
		Rename:         os.Rename,
		SmokeTest:      runSmokeTest,
	}
	u.HTTP = newUpdateHTTPClient(cfg.Insecure)
	return u
}

// newUpdateHTTPClient refuses to follow a redirect from HTTPS to anything else, so a
// compromised or misconfigured redirect cannot downgrade the transport mid-download.
func newUpdateHTTPClient(insecure bool) *http.Client {
	return &http.Client{
		Timeout: updateHTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && !insecure {
				return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
			}
			return nil
		},
	}
}

// resolveExecutable returns the running binary's path with symlinks resolved, which is
// the file a rename must replace (replacing a symlink would leave the target stale).
func resolveExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// runSmokeTest executes the staged binary with -version, with a deadline and a minimal
// environment. It is only ever called on a file whose checksum already matched.
func runSmokeTest(ctx context.Context, binary string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, smokeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-version")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	return string(out), err
}

// UpdateResult describes a completed Run.
type UpdateResult struct {
	UpToDate bool
	From, To string
	Path     string
}

// Run performs the update. On any error the installed binary is untouched.
func (u *Updater) Run(ctx context.Context) (*UpdateResult, error) {
	if u.GOOS != "linux" || (u.GOARCH != "amd64" && u.GOARCH != "arm64") {
		return nil, fail(exitUnsupported, "no release artifact is published for %s/%s (supported: linux/amd64, linux/arm64)", u.GOOS, u.GOARCH)
	}
	if err := u.Config.validateEndpoint(); err != nil {
		return nil, fail(exitConfig, "configuration: %v", err)
	}
	endpoint := strings.TrimRight(u.Config.Endpoint, "/")

	installed, err := parseAgentVersion(u.CurrentVersion)
	if err != nil {
		return nil, fail(exitGeneric, "this binary has an unparseable version: %v", err)
	}

	exePath, err := u.Executable()
	if err != nil {
		return nil, fail(exitGeneric, "cannot determine the running executable: %v", err)
	}

	// Ask the same endpoint the agent already pushes to what "latest" is. This is the
	// only release-discovery path; the dashboard badge and install.sh derive from the
	// same server-side value.
	client := &Client{endpoint: endpoint, http: u.HTTP}
	info, err := client.FetchVersion(ctx)
	if err != nil {
		return nil, fail(exitRelease, "could not determine the latest release: %v", err)
	}
	latest, err := parseAgentVersion(info.Version)
	if err != nil {
		return nil, fail(exitRelease, "server advertised an unusable version %q: %v", info.Version, err)
	}

	if compareAgentVersions(installed, latest) >= 0 {
		return &UpdateResult{UpToDate: true, From: u.CurrentVersion, To: formatAgentVersion(installed), Path: exePath}, nil
	}

	// From here on the executable path matters, so surface a path the service probably
	// does not use before doing any work.
	if exePath != installedBinaryPath {
		fmt.Fprintf(u.ErrOut, "warning: replacing %s, but the systemd service normally runs %s; the service keeps using whichever file its unit points at\n", exePath, installedBinaryPath)
	}

	asset := fmt.Sprintf("serveragent-%s-linux-%s", formatAgentVersion(latest), u.GOARCH)
	sumsBody, err := u.fetch(ctx, endpoint+distPath+"SHA256SUMS", maxChecksumsBytes)
	if err != nil {
		return nil, err
	}
	want, err := checksumFor(sumsBody, asset)
	if err != nil {
		return nil, fail(exitVerify, "%v", err)
	}

	// Stage next to the target so the final rename is atomic. Creating the file before
	// downloading also surfaces an unwritable directory before any bytes are fetched.
	dir := filepath.Dir(exePath)
	tmp, err := u.CreateTemp(dir, ".serveragent-update-*")
	if err != nil {
		return nil, writeFailure("cannot create a staging file in "+dir, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	got, err := u.download(ctx, endpoint+distPath+asset, tmp)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fail(exitVerify, "checksum mismatch for %s: expected %s, got %s; nothing was installed", asset, want, got)
	}
	if err := tmp.Sync(); err != nil {
		return nil, writeFailure("cannot flush the staged binary", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, writeFailure("cannot close the staged binary", err)
	}

	mode := os.FileMode(0o755)
	if st, err := os.Stat(exePath); err == nil {
		if perm := st.Mode().Perm(); perm&0o111 != 0 {
			mode = perm
		}
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return nil, writeFailure("cannot set permissions on the staged binary", err)
	}

	// The checksum proves the file is the published artifact; this proves it runs on
	// this host and is the release we meant to fetch (wrong libc, wrong arch, or a
	// mislabelled upload fails here instead of after the swap).
	out, err := u.SmokeTest(ctx, tmpPath)
	if err != nil {
		return nil, fail(exitVerify, "the downloaded binary failed its self-check: %v; nothing was installed", err)
	}
	reported, ok := parseVersionOutput(out)
	if !ok || reported != formatAgentVersion(latest) {
		return nil, fail(exitVerify, "the downloaded binary reports %q, expected version %s; nothing was installed", strings.TrimSpace(out), formatAgentVersion(latest))
	}

	if err := u.Rename(tmpPath, exePath); err != nil {
		return nil, writeFailure("cannot replace "+exePath, err)
	}
	committed = true

	return &UpdateResult{From: u.CurrentVersion, To: formatAgentVersion(latest), Path: exePath}, nil
}

// writeFailure classifies a local write error, pointing at sudo for the common case.
func writeFailure(what string, err error) *updateError {
	if errors.Is(err, fs.ErrPermission) {
		return fail(exitPermission, "%s: %v (run with sudo; the installed binary is owned by root)", what, err)
	}
	return fail(exitPermission, "%s: %v", what, err)
}

// fetch GETs url into memory, mapping every unavailability to exitRelease.
func (u *Updater) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fail(exitRelease, "reading %s: %v", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fail(exitVerify, "%s is larger than the %d byte limit", url, limit)
	}
	return body, nil
}

// download streams the artifact into dst and returns its SHA-256 as hex.
func (u *Updater) download(ctx context.Context, url string, dst io.Writer) (string, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxBinaryBytes {
		return "", fail(exitVerify, "%s is larger than the %d byte limit", url, maxBinaryBytes)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, maxBinaryBytes+1))
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return "", writeFailure("cannot write the staged binary", err)
		}
		return "", fail(exitRelease, "download interrupted: %v", err)
	}
	if n > maxBinaryBytes {
		return "", fail(exitVerify, "%s is larger than the %d byte limit", url, maxBinaryBytes)
	}
	if n == 0 {
		return "", fail(exitRelease, "%s returned an empty body", url)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get issues a GET and treats any non-200 as the release being unavailable.
func (u *Updater) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fail(exitGeneric, "building request: %v", err)
	}
	req.Header.Set("User-Agent", "statuspage-serveragent/"+AgentVersion)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, fail(exitRelease, "GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fail(exitRelease, "%s was not found (404): the release is not published yet", path.Base(url))
		}
		return nil, fail(exitRelease, "GET %s returned %d", url, resp.StatusCode)
	}
	return resp, nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// checksumFor finds asset in a `shasum -a 256` style listing ("<hex>  <name>", with an
// optional "*" binary marker). A missing entry, a malformed hash, or two entries that
// disagree are all errors: this is the integrity decision, so ambiguity must not pass.
func checksumFor(sums []byte, asset string) (string, error) {
	found := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != asset {
			continue
		}
		if !sha256Hex.MatchString(fields[0]) {
			return "", fmt.Errorf("SHA256SUMS has a malformed checksum for %s", asset)
		}
		sum := strings.ToLower(fields[0])
		if found != "" && found != sum {
			return "", fmt.Errorf("SHA256SUMS lists conflicting checksums for %s", asset)
		}
		found = sum
	}
	if found == "" {
		return "", fmt.Errorf("no checksum is published for %s", asset)
	}
	return found, nil
}

// agentVersion is a strict MAJOR.MINOR.PATCH triple. The release process only produces
// plain triples, so pre-release and build suffixes are rejected rather than guessed at.
type agentVersion [3]int

// parseAgentVersion accepts "1.2.3" or "v1.2.3" and nothing else.
func parseAgentVersion(s string) (agentVersion, error) {
	var v agentVersion
	raw := strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("invalid version %q: want MAJOR.MINOR.PATCH", s)
	}
	for i, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return v, fmt.Errorf("invalid version %q: %q is not a plain number", s, p)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, fmt.Errorf("invalid version %q: %w", s, err)
		}
		v[i] = n
	}
	return v, nil
}

// compareAgentVersions compares numerically (0.1.10 > 0.1.9), returning -1, 0 or 1.
func compareAgentVersions(a, b agentVersion) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func formatAgentVersion(v agentVersion) string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

var versionOutput = regexp.MustCompile(`^statuspage-serveragent (\S+) \(`)

// parseVersionOutput extracts the version from `serveragent -version` output and
// normalises it, so "0.1.3" and "v0.1.3" compare equal.
func parseVersionOutput(out string) (string, bool) {
	m := versionOutput.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", false
	}
	v, err := parseAgentVersion(m[1])
	if err != nil {
		return "", false
	}
	return formatAgentVersion(v), true
}

// runUpdate is the entry point for the -update flag. It returns the process exit code.
func runUpdate(configPath string, out, errOut io.Writer) int {
	return runUpdateWith(configPath, out, errOut, nil)
}

// runUpdateWith is runUpdate with a hook to adjust the Updater before it runs, so tests
// can point it at a temp directory instead of the real executable.
func runUpdateWith(configPath string, out, errOut io.Writer, tweak func(*Updater)) int {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		hint := ""
		if errors.Is(err, fs.ErrPermission) {
			hint = " (the config file is root-readable only; run with sudo)"
		}
		fmt.Fprintf(errOut, "serveragent: configuration: %v%s\n", err, hint)
		return exitConfig
	}

	ctx, cancel := context.WithTimeout(context.Background(), updateHTTPTimeout+time.Minute)
	defer cancel()

	u := NewUpdater(cfg, out, errOut)
	if tweak != nil {
		tweak(u)
	}
	res, err := u.Run(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "serveragent: update failed: %v\n", err)
		return updateCode(err)
	}
	if res.UpToDate {
		fmt.Fprintf(out, "serveragent is already up to date (v%s)\n", res.To)
		return exitOK
	}
	fmt.Fprintf(out, "Updated serveragent v%s -> v%s.\n", res.From, res.To)
	fmt.Fprintf(out, "Replaced executable: %s\n", res.Path)
	fmt.Fprintf(out, "Restart the service to start using the new version:\n  sudo systemctl restart %s\n", serviceName)
	return exitOK
}
