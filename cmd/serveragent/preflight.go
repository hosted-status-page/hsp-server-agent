package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	// serviceUnitPath is the systemd unit install.sh writes. Its presence is what marks an
	// installation as the supported systemd one.
	serviceUnitPath = "/etc/systemd/system/" + serviceName + ".service"
)

var (
	// errNotRoot means the service identity cannot be assumed because this process is not
	// root. It is an actionable condition (use sudo), not a pass.
	errNotRoot = errors.New("not running as root")
	// errAccessUnverifiable means the check could not be carried out for another reason.
	errAccessUnverifiable = errors.New("cannot act as the service user")
)

// serviceConfigCheck verifies, before anything is replaced, that the service account
// systemd runs the service as can open the config file the service is started with.
//
// Checking as root would prove nothing: the failure this exists for is a service that is
// replaced and restarted successfully and then exits because its own, unprivileged
// identity cannot read its config (for example after a parent directory was tightened to
// 0700). That only shows up on the next restart, which is the worst moment to learn it.
//
// It fails closed. For an installation managed by the installer's systemd unit, anything
// that stops it from proving the service identity can read the config (systemd cannot be
// asked, the unit sets no User=, the account is unknown, this process is not root, the
// access test cannot run) is an error and nothing is replaced. The single exception is a
// binary with no such unit at all: that is a standalone installation, not the supported
// one, there is no service identity to check, and the check says so instead of pretending.
type serviceConfigCheck struct {
	UnitPath   string // the supported installation's systemd unit; absent means standalone
	ConfigPath string // the -config value this process was given, used when systemd names none
	ErrOut     io.Writer

	// Seams, so every outcome can be exercised without being root or touching /etc.
	Stat       func(path string) (fs.FileInfo, error)
	LookupUser func(name string) (*user.User, error)
	// Show returns systemd's effective value (drop-ins included) of a unit property.
	Show func(ctx context.Context, property string) (string, error)
	// Access reports whether usr passes `test <flag> <path>`; flag is "-r" or "-x".
	Access func(ctx context.Context, usr *user.User, flag, path string) (bool, error)
}

func newServiceConfigCheck(configPath string, errOut io.Writer) *serviceConfigCheck {
	return &serviceConfigCheck{
		UnitPath:   serviceUnitPath,
		ConfigPath: configPath,
		ErrOut:     errOut,
		Stat:       os.Stat,
		LookupUser: user.Lookup,
		Show:       systemctlShow,
		Access:     accessAsUser,
	}
}

// Run returns nil only when the service user is proven able to read the config (or the
// installation is standalone), and an *updateError describing the exact cause otherwise.
func (c *serviceConfigCheck) Run(ctx context.Context) error {
	if _, err := c.Stat(c.UnitPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(c.ErrOut, "note: %s not found, so this is a standalone binary rather than the systemd service the installer sets up; "+
				"there is no service account whose access to the config can be checked\n", c.UnitPath)
			return nil
		}
		return fail(exitGeneric, "cannot inspect %s: %v; nothing was changed", c.UnitPath, err)
	}

	svcUser, err := c.Show(ctx, "User")
	if err != nil {
		return fail(exitGeneric, "cannot ask systemd which account runs %s (%v), so its access to the config cannot be checked; nothing was changed", serviceName, err)
	}
	if svcUser == "" {
		return fail(exitConfig, "%s sets no User=, so it would run as root; that is not the supported installation and the config check cannot be made for it; nothing was changed", c.UnitPath)
	}
	svcGroup, err := c.Show(ctx, "Group")
	if err != nil || svcGroup == "" {
		svcGroup = svcUser
	}
	execStart, err := c.Show(ctx, "ExecStart")
	if err != nil {
		return fail(exitGeneric, "cannot ask systemd for the %s command line (%v), so the config path cannot be confirmed; nothing was changed", serviceName, err)
	}
	configPath := configFromExecStart(execStart)
	if configPath == "" {
		configPath = c.ConfigPath
	}
	if configPath == "" {
		return fail(exitConfig, "the config path the service reads could not be determined; nothing was changed")
	}

	usr, err := c.LookupUser(svcUser)
	if err != nil {
		return fail(exitConfig, "the service user %q does not exist (%v); nothing was changed", svcUser, err)
	}

	if _, err := c.Stat(configPath); err != nil && errors.Is(err, fs.ErrNotExist) {
		return fail(exitConfig, "%s does not exist; nothing was changed", configPath)
	}

	ok, err := c.Access(ctx, usr, "-r", configPath)
	switch {
	case errors.Is(err, errNotRoot):
		return fail(exitPermission, "run this with sudo: switching to the service user %q to confirm it can read %s needs root; nothing was changed", svcUser, configPath)
	case err != nil:
		return fail(exitGeneric, "could not confirm that the service user %q can read %s (%v); nothing was changed", svcUser, configPath, err)
	case ok:
		return nil
	}
	return fail(exitConfig, "%s", c.explain(ctx, usr, svcUser, svcGroup, configPath))
}

// explain names the cause of a denied read: the outermost directory the service user
// cannot search if there is one, otherwise the file's own ownership and mode. It always
// states that nothing was changed and never prints file contents.
func (c *serviceConfigCheck) explain(ctx context.Context, usr *user.User, svcUser, svcGroup, configPath string) string {
	var blocked string
	for dir := filepath.Dir(configPath); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if ok, err := c.Access(ctx, usr, "-x", dir); err == nil && !ok {
			blocked = dir // keep walking up: the outermost one is the one to fix
		}
	}
	if blocked != "" {
		return fmt.Sprintf("the service user %q cannot read %s because it cannot enter %s (%s); nothing was changed. "+
			"Grant that user traversal only, for example: sudo setfacl -m u:%s:x %s -- then run the update again",
			svcUser, configPath, blocked, c.describe(blocked), svcUser, blocked)
	}
	return fmt.Sprintf("the service user %q cannot read %s (currently %s); nothing was changed. "+
		"Make it readable by the service group, for example: sudo chgrp %s %s && sudo chmod 0640 %s -- then run the update again",
		svcUser, configPath, c.describe(configPath), svcGroup, configPath, configPath)
}

// describe renders "owner:group mode" for diagnostics, falling back to numeric ids.
func (c *serviceConfigCheck) describe(path string) string {
	fi, err := c.Stat(path)
	if err != nil {
		return "state unknown"
	}
	mode := fmt.Sprintf("%04o", fi.Mode().Perm())
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		owner, group := strconv.FormatUint(uint64(st.Uid), 10), strconv.FormatUint(uint64(st.Gid), 10)
		if u, err := user.LookupId(owner); err == nil {
			owner = u.Username
		}
		if g, err := user.LookupGroupId(group); err == nil {
			group = g.Name
		}
		return owner + ":" + group + " " + mode
	}
	return "mode " + mode
}

// configFromExecStart returns the path following -config / --config in the command line
// of a `systemctl show -p ExecStart` value, which looks like
// "{ path=/usr/local/bin/serveragent ; argv[]=/usr/local/bin/serveragent -config /etc/x.conf ; ignore_errors=no ; ... }".
func configFromExecStart(value string) string {
	for _, line := range strings.Split(value, "\n") {
		_, rest, ok := strings.Cut(line, "argv[]=")
		if !ok {
			continue
		}
		argv, _, _ := strings.Cut(rest, " ; ")
		if path := configArgument(strings.TrimSuffix(strings.TrimSpace(argv), "}")); path != "" {
			return path
		}
	}
	return ""
}

// configArgument returns the path given to -config / --config in a command line.
func configArgument(commandLine string) string {
	fields := strings.Fields(commandLine)
	for i, f := range fields {
		for _, flag := range []string{"-config", "--config"} {
			if f == flag && i+1 < len(fields) {
				return fields[i+1]
			}
			if strings.HasPrefix(f, flag+"=") {
				return strings.TrimPrefix(f, flag+"=")
			}
		}
	}
	return ""
}

// accessAsUser runs `test <flag> <path>` with usr's uid, gid and supplementary groups, so
// the kernel makes the decision with the service's real credentials. Switching identity
// needs root; anything else is reported as unverifiable rather than guessed at.
func accessAsUser(ctx context.Context, usr *user.User, flag, path string) (bool, error) {
	if os.Geteuid() != 0 {
		return false, errNotRoot
	}
	uid, err := strconv.ParseUint(usr.Uid, 10, 32)
	if err != nil {
		return false, fmt.Errorf("%w: non-numeric uid %q", errAccessUnverifiable, usr.Uid)
	}
	gid, err := strconv.ParseUint(usr.Gid, 10, 32)
	if err != nil {
		return false, fmt.Errorf("%w: non-numeric gid %q", errAccessUnverifiable, usr.Gid)
	}
	var groups []uint32
	if ids, err := usr.GroupIds(); err == nil {
		for _, id := range ids {
			if n, err := strconv.ParseUint(id, 10, 32); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	testBin, err := exec.LookPath("test")
	if err != nil {
		return false, fmt.Errorf("%w: no test binary", errAccessUnverifiable)
	}

	ctx, cancel := context.WithTimeout(ctx, smokeTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, testBin, flag, path)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uint32(uid), Gid: uint32(gid), Groups: groups,
	}}
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("%w: %v", errAccessUnverifiable, err)
}

// systemctlShow returns the effective value of a property of the service unit, drop-ins
// included, which is what systemd will actually start the service with.
func systemctlShow(ctx context.Context, property string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, smokeTestTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", serviceName+".service", "-p", property, "--value").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
