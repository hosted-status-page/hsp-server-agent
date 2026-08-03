#!/usr/bin/env bash
#
# Installs the StatusPage.me Server Agent.
#
# The agent collects host metrics (CPU, memory, load, disk, network) and pushes them to
# your StatusPage.me account every 60 seconds. It is read-only: it never executes
# commands sent by the server, and never downloads or runs code after installation.
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
# Piping a script to a root shell requires trusting the source. If you would rather not,
# download it first, read it, then run it — it is written to be readable.

set -euo pipefail

ENDPOINT="https://statuspage.me"
SERVER_ID=""
INGEST_KEY=""
HOSTNAME_OVERRIDE=""
HOSTNAME_SET=0
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

Required:
  --server-id <uuid>     Server ID from your StatusPage.me dashboard
  --ingest-key <key>     Ingest key, shown once when the server was added

Options:
  --endpoint <url>       API endpoint (default: https://statuspage.me)
  --hostname <name>      Hostname to report. Pass an empty string to report none:
                         hostnames often contain a person's name, and nothing requires one.
  --interval <seconds>   Collection interval (default: 60)
  --version <version>    Agent version to install (default: 0.1.0)
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
        --endpoint)   ENDPOINT="${2:-}"; shift 2 ;;
        --hostname)   HOSTNAME_OVERRIDE="${2:-}"; HOSTNAME_SET=1; shift 2 ;;
        --interval)   INTERVAL="${2:-}"; shift 2 ;;
        --version)    VERSION="${2:-}"; shift 2 ;;
        --uninstall)  uninstall ;;
        -h|--help)    usage; exit 0 ;;
        *) die "unknown option: $1 (try --help)" ;;
    esac
done

require_root
require_systemd

[ -n "${SERVER_ID}" ]  || die "--server-id is required (find it in your dashboard)"
[ -n "${INGEST_KEY}" ] || die "--ingest-key is required (shown once when the server was added)"

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
mkdir -p "${CONFIG_DIR}"
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
