#!/usr/bin/env bash
#
# Installs the StatusPage.me Server Agent.
#
# The agent collects host metrics (CPU, memory, load, disk, network) and pushes them to
# your StatusPage.me account every 60 seconds. It is read-only: it never executes
# commands sent by the server, and never downloads or runs code on its own. Updating is
# something you do on purpose, either with 'serveragent --update' or by running this
# script again with --upgrade.
#
# Run 'serveragent -metrics' after installing to print the complete list of what it
# collects, or 'serveragent -dry-run' to see a real sample from this host.
#
# Source: https://github.com/hosted-status-page/hsp-server-agent
#
# Usage:
#   curl -fsSL https://statuspage.me/install-server-agent.sh | sudo bash -s -- \
#     --server-id <uuid> --ingest-key <key>
#
# To update an existing install without the ingest key (agents older than the ones that
# have 'serveragent --update'):
#   curl -fsSL https://statuspage.me/install-server-agent.sh | sudo bash -s -- --upgrade
#
# Piping a script to a root shell requires trusting the source. If you would rather not,
# download it first, read it, then run it — it is written to be readable.

set -euo pipefail

ENDPOINT="https://statuspage.me"
SERVER_ID=""
INGEST_KEY=""
HOSTNAME_OVERRIDE=""
HOSTNAME_SET=0
ENDPOINT_SET=0
UPGRADE=0
# Placeholder: the release version is the git tag, and the StatusPage.me server stamps this
# line with it when it publishes the installer, so there is nothing to bump by hand.
VERSION="0.1.0"
INTERVAL="60"

INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/statuspage"
CONFIG_FILE="${CONFIG_DIR}/serveragent.conf"
SPOOL_DIR="/var/lib/statuspage"
SERVICE_FILE="/etc/systemd/system/statuspage-serveragent.service"
SERVICE_USER="statuspage-agent"

usage() {
    cat <<'USAGE'
Usage: install-server-agent.sh --server-id <uuid> --ingest-key <key> [options]
       install-server-agent.sh --upgrade [--endpoint <url>] [--version <version>]

Required:
  --server-id <uuid>     Server ID from your StatusPage.me dashboard
  --ingest-key <key>     Ingest key, shown once when the server was added

Options:
  --endpoint <url>       API endpoint (default: https://statuspage.me)
  --hostname <name>      Hostname to report. Pass an empty string to report none:
                         hostnames often contain a person's name, and nothing requires one.
  --interval <seconds>   Collection interval (default: 60)
  --version <version>    Agent version to install (default: the release this script was published with)
  --upgrade              Replace the binary of an existing install and restart the service.
                         Keeps the existing config, ingest key, service unit and spool.
                         Needs no --server-id or --ingest-key.
  --uninstall            Remove the agent, its config, and its buffered data
  -h, --help             Show this help

Environment:
  Anything set in the config file can be overridden with SP_* environment variables.
USAGE
}

log()  { printf '\033[0;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[0;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[0;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
    [ "$(id -u)" -eq 0 ] || die "this script must run as root (try: sudo $0 ...)"
}

detect_arch() {
    local machine
    machine="$(uname -m)"
    case "${machine}" in
        x86_64|amd64)  echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) die "unsupported architecture: ${machine} (supported: x86_64, aarch64)" ;;
    esac
}

require_systemd() {
    [ "$(uname -s)" = "Linux" ] || die "this installer supports Linux only; this host reports $(uname -s)"
    command -v systemctl >/dev/null 2>&1 || die "systemd is required but systemctl was not found"
}

# preflight:begin
# The service runs as an unprivileged user, so "root can read the config" proves nothing.
# What matters is whether that user can open the file: the file's own owner, group and mode
# have to allow it, and so does every directory on the way there. A parent directory that
# was later tightened (for example to 0700) makes a running agent look healthy until its
# next restart, when it exits with "permission denied".
#
# This check runs before anything is replaced or restarted and fails closed: if it cannot
# prove that the service identity can read the config, it stops. It never guesses, never
# continues with a warning, and never changes permissions.

# unit_property KEY prints systemd's effective value for the service unit, drop-ins
# included. It fails when systemd cannot be queried.
unit_property() {
    systemctl show statuspage-serveragent.service -p "$1" --value
}

# run_as_user USER COMMAND... runs COMMAND with USER's uid, gid and supplementary groups.
# Returns 125 when this host has no tool to switch identity.
run_as_user() {
    local user="$1"
    shift
    if command -v runuser >/dev/null 2>&1; then
        runuser -u "${user}" -- "$@"
    elif command -v setpriv >/dev/null 2>&1; then
        setpriv --reuid "${user}" --regid "$(id -g "${user}")" --init-groups -- "$@"
    else
        return 125
    fi
}

# exec_start_config prints the path following -config in the unit's effective ExecStart,
# or nothing when there is none.
exec_start_config() {
    local argv
    argv="$(unit_property ExecStart 2>/dev/null | sed -n 's/.*argv\[\]=\([^;]*\);.*/\1/p' | head -n 1)"
    # shellcheck disable=SC2086
    set -- ${argv}
    while [ $# -gt 0 ]; do
        case "$1" in
            -config|--config) [ $# -ge 2 ] && echo "$2"; return 0 ;;
            -config=*|--config=*) echo "${1#*=}"; return 0 ;;
        esac
        shift
    done
}

# preflight_config_access MODE aborts unless the service user can read the config. MODE is
# "upgrade" (an installed service is about to be replaced and restarted) or "install" (the
# unit this script writes has not been created yet, so the identity is the one it will write).
preflight_config_access() {
    local mode="$1" svc_user svc_group cfg rc=0 dir blocked="" untouched
    if [ "${mode}" = "upgrade" ]; then
        untouched="The installed binary, configuration and service were not touched."
        svc_user="$(unit_property User)" \
            || die "cannot ask systemd which account runs the service (systemctl show failed), so the config cannot be checked. ${untouched}"
        [ -n "${svc_user}" ] \
            || die "the service unit sets no User=, which means it would run as root. That is not the supported installation, and the config check cannot be made for it. ${untouched}"
        svc_group="$(unit_property Group)" || svc_group=""
        cfg="$(exec_start_config)"
    else
        untouched="The service was not started."
        svc_user="${SERVICE_USER}"
        svc_group="${SERVICE_USER}"
        cfg=""
    fi
    [ -n "${svc_group}" ] || svc_group="${svc_user}"
    [ -n "${cfg}" ] || cfg="${CONFIG_FILE}"

    id "${svc_user}" >/dev/null 2>&1 \
        || die "the service user '${svc_user}' does not exist. ${untouched}"
    [ -f "${cfg}" ] || die "${cfg} does not exist. ${untouched}"

    run_as_user "${svc_user}" test -r "${cfg}" || rc=$?
    case "${rc}" in
        0) return 0 ;;
        1) ;;
        *)
            die "could not check whether '${svc_user}' can read ${cfg}: this host has no working runuser or setpriv (util-linux), or switching to that account failed (exit ${rc}). Install util-linux, or confirm the permissions by hand and re-run. ${untouched}"
            ;;
    esac

    # Name the outermost directory the service user cannot search, if that is the cause.
    dir="$(dirname "${cfg}")"
    while [ -n "${dir}" ] && [ "${dir}" != "/" ] && [ "${dir}" != "." ]; do
        run_as_user "${svc_user}" test -x "${dir}" || blocked="${dir}"
        dir="$(dirname "${dir}")"
    done

    if [ -n "${blocked}" ]; then
        die "the service user '${svc_user}' cannot read ${cfg} because it cannot enter ${blocked} ($(stat -c '%U:%G %a' "${blocked}")). ${untouched} Grant that user traversal only, for example: setfacl -m u:${svc_user}:x ${blocked} -- then run this command again."
    fi
    die "the service user '${svc_user}' cannot read ${cfg} (currently $(stat -c '%U:%G %a' "${cfg}")). ${untouched} Make it readable by the service group, for example: chgrp ${svc_group} ${cfg} && chmod 0640 ${cfg} -- then run this command again."
}
# preflight:end

uninstall() {
    require_root
    log "stopping and disabling the service"
    systemctl stop statuspage-serveragent.service 2>/dev/null || true
    systemctl disable statuspage-serveragent.service 2>/dev/null || true

    log "removing files"
    rm -f "${SERVICE_FILE}"
    rm -f "${INSTALL_DIR}/serveragent"
    rm -rf "${CONFIG_DIR}"
    rm -rf "${SPOOL_DIR}"

    systemctl daemon-reload

    if id "${SERVICE_USER}" >/dev/null 2>&1; then
        userdel "${SERVICE_USER}" 2>/dev/null || warn "could not remove the ${SERVICE_USER} user"
    fi

    log "the Server Agent has been removed"
    exit 0
}

while [ $# -gt 0 ]; do
    case "$1" in
        --server-id)  SERVER_ID="${2:-}"; shift 2 ;;
        --ingest-key) INGEST_KEY="${2:-}"; shift 2 ;;
        --endpoint)   ENDPOINT="${2:-}"; ENDPOINT_SET=1; shift 2 ;;
        --hostname)   HOSTNAME_OVERRIDE="${2:-}"; HOSTNAME_SET=1; shift 2 ;;
        --interval)   INTERVAL="${2:-}"; shift 2 ;;
        --version)    VERSION="${2:-}"; shift 2 ;;
        --upgrade)    UPGRADE=1; shift ;;
        --uninstall)  uninstall ;;
        -h|--help)    usage; exit 0 ;;
        *) die "unknown option: $1 (try --help)" ;;
    esac
done

require_root
require_systemd

if [ "${UPGRADE}" -eq 1 ]; then
    # An upgrade only swaps the binary. Settings that belong to a fresh install would be
    # silently ignored, so refuse them rather than let someone think they took effect.
    [ -z "${SERVER_ID}${INGEST_KEY}" ] && [ "${HOSTNAME_SET}" -eq 0 ] \
        || die "--upgrade keeps the existing configuration; do not combine it with --server-id, --ingest-key or --hostname"
    [ -f "${CONFIG_FILE}" ] && [ -f "${SERVICE_FILE}" ] \
        || die "no existing installation found (expected ${CONFIG_FILE} and ${SERVICE_FILE}); install with --server-id and --ingest-key instead"
    if [ "${ENDPOINT_SET}" -eq 0 ]; then
        # Download from wherever this host was installed from, not the compiled-in default.
        CONFIGURED_ENDPOINT="$(sed -n 's/^SP_ENDPOINT=//p' "${CONFIG_FILE}" | head -n 1 | tr -d "\"' ")"
        [ -z "${CONFIGURED_ENDPOINT}" ] || ENDPOINT="${CONFIGURED_ENDPOINT%/}"
    fi
    # Before downloading, let alone replacing, anything: a binary that cannot read its own
    # config would be swapped in and then fail on the restart.
    preflight_config_access upgrade
else
    [ -n "${SERVER_ID}" ]  || die "--server-id is required (find it in your dashboard)"
    [ -n "${INGEST_KEY}" ] || die "--ingest-key is required (shown once when the server was added)"
fi

ARCH="$(detect_arch)"
ASSET="serveragent-${VERSION}-linux-${ARCH}"
DOWNLOAD_URL="${ENDPOINT}/dist/serveragent/${ASSET}"
CHECKSUM_URL="${ENDPOINT}/dist/serveragent/SHA256SUMS"

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

log "downloading ${ASSET}"
curl -fsSL "${DOWNLOAD_URL}" -o "${TMP_DIR}/serveragent" \
    || die "download failed from ${DOWNLOAD_URL}"

# Verify before anything is installed. A binary that runs as a service on this host is
# not something to install on the strength of a TLS connection alone.
log "verifying checksum"
if curl -fsSL "${CHECKSUM_URL}" -o "${TMP_DIR}/SHA256SUMS" 2>/dev/null; then
    EXPECTED="$(grep " ${ASSET}\$" "${TMP_DIR}/SHA256SUMS" | awk '{print $1}')"
    [ -n "${EXPECTED}" ] || die "no checksum published for ${ASSET}"
    ACTUAL="$(sha256sum "${TMP_DIR}/serveragent" | awk '{print $1}')"
    [ "${EXPECTED}" = "${ACTUAL}" ] \
        || die "checksum mismatch: expected ${EXPECTED}, got ${ACTUAL}. Aborting."
    log "checksum verified"
else
    die "could not fetch ${CHECKSUM_URL}; refusing to install an unverified binary"
fi

if [ "${UPGRADE}" -eq 1 ]; then
    # Stage beside the target and rename, so the binary is replaced atomically and a
    # failed copy can never leave a half-written executable behind.
    log "replacing ${INSTALL_DIR}/serveragent"
    STAGED="${INSTALL_DIR}/.serveragent.upgrade.$$"
    trap 'rm -rf "${TMP_DIR}" "${STAGED}"' EXIT
    install -m 0755 "${TMP_DIR}/serveragent" "${STAGED}"
    mv -f "${STAGED}" "${INSTALL_DIR}/serveragent"

    log "restarting the service"
    systemctl restart statuspage-serveragent.service
    sleep 3
    if systemctl is-active --quiet statuspage-serveragent.service; then
        log "the Server Agent was upgraded to ${VERSION} and is running"
        exit 0
    fi
    warn "the service did not start cleanly after the upgrade"
    journalctl -u statuspage-serveragent -n 20 --no-pager || true
    exit 1
fi

log "creating the ${SERVICE_USER} service account"
if ! id "${SERVICE_USER}" >/dev/null 2>&1; then
    # A dedicated account with no login shell and no home directory. The agent needs to
    # read /proc and /sys, not to be root.
    useradd --system --no-create-home --shell /usr/sbin/nologin "${SERVICE_USER}" \
        || die "failed to create the ${SERVICE_USER} user"
fi

log "installing the binary to ${INSTALL_DIR}/serveragent"
install -m 0755 "${TMP_DIR}/serveragent" "${INSTALL_DIR}/serveragent"

log "writing ${CONFIG_FILE}"
# Create the directory with an explicit mode so a restrictive umask cannot make it
# unsearchable for the service user. An existing directory is left exactly as it is: it may
# be shared and deliberately hardened, and the preflight below reports a problem with it.
[ -d "${CONFIG_DIR}" ] || install -d -m 0755 -o root -g root "${CONFIG_DIR}"
{
    echo "# StatusPage.me Server Agent configuration"
    echo "# Generated by install-server-agent.sh on $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
    echo ""
    echo "SP_ENDPOINT=${ENDPOINT}"
    echo "SP_SERVER_ID=${SERVER_ID}"
    echo "SP_INGEST_KEY=${INGEST_KEY}"
    echo "SP_INTERVAL_SECONDS=${INTERVAL}"
    echo "SP_SPOOL_PATH=${SPOOL_DIR}/serveragent-spool.jsonl"
    if [ "${HOSTNAME_SET}" -eq 1 ]; then
        echo ""
        echo "# Reported hostname. Empty means none is sent."
        echo "SP_HOSTNAME=${HOSTNAME_OVERRIDE}"
    fi
} > "${CONFIG_FILE}"

# The file holds the ingest key, so it must not be world-readable.
chown root:"${SERVICE_USER}" "${CONFIG_FILE}"
chmod 0640 "${CONFIG_FILE}"
preflight_config_access install

log "creating the spool directory ${SPOOL_DIR}"
mkdir -p "${SPOOL_DIR}"
chown "${SERVICE_USER}":"${SERVICE_USER}" "${SPOOL_DIR}"
chmod 0750 "${SPOOL_DIR}"

log "writing ${SERVICE_FILE}"
# The hardening directives below are the point of running an agent at all: it needs to
# read system counters and nothing else. ProtectSystem/ProtectHome make the filesystem
# read-only apart from its own spool, and the syscall and capability restrictions mean a
# compromise of this process is not a foothold on the host.
cat > "${SERVICE_FILE}" <<SERVICE
[Unit]
Description=StatusPage.me Server Agent
Documentation=https://github.com/hosted-status-page/hsp-server-agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
ExecStart=${INSTALL_DIR}/serveragent -config ${CONFIG_FILE}
Restart=always
RestartSec=30

NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
CapabilityBoundingSet=
AmbientCapabilities=
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
ReadWritePaths=${SPOOL_DIR}

MemoryMax=128M
CPUQuota=10%

[Install]
WantedBy=multi-user.target
SERVICE

log "enabling and starting the service"
systemctl daemon-reload
systemctl enable statuspage-serveragent.service >/dev/null 2>&1
systemctl restart statuspage-serveragent.service

sleep 3
if systemctl is-active --quiet statuspage-serveragent.service; then
    log "the Server Agent is running"
    echo ""
    echo "  Status:  systemctl status statuspage-serveragent"
    echo "  Logs:    journalctl -u statuspage-serveragent -f"
    echo "  Metrics: serveragent -metrics     (exactly what it collects)"
    echo "  Sample:  serveragent -dry-run     (a real sample from this host)"
    echo "  Remove:  $0 --uninstall"
    echo ""
    echo "Metrics should appear in your dashboard within a couple of minutes."
else
    warn "the service did not start cleanly"
    echo ""
    journalctl -u statuspage-serveragent -n 20 --no-pager || true
    exit 1
fi
