# hsp-server-agent

The host metrics agent for [StatusPage.me](https://statuspage.me). You install it on your
own servers; it collects CPU, memory, load, disk, and network every 60 seconds and pushes
them to your account, where they render on the same timeline as your incidents.

This repository exists because asking someone to install a binary on their production
server, as root, is asking for trust. The source is the answer to "what does it actually
send?"

```bash
go install github.com/hosted-status-page/hsp-server-agent/cmd/serveragent@latest
serveragent -metrics    # the complete list of what it collects
serveragent -dry-run    # a real sample from this machine, printed, not sent
```

> **Not the same "agent" as our regional probes.** Elsewhere you may see us refer to
> monitoring agents — those are machines *we* run, making outbound checks against your
> endpoints. This is the opposite direction: your machine, your install, pushing inbound.

## Why host metrics

Uptime monitoring tells you *that* something broke. Host metrics on the same chart tell
you *why*. When an API goes down at 06:33 and the load average spiked at 06:29, that
correlation is the answer — and putting both on one timeline is the point of the feature
this agent feeds.

## What it collects

| Group   | Metrics                                                                                                                                      |
|---------|----------------------------------------------------------------------------------------------------------------------------------------------|
| CPU     | `cpu_user_pct`, `cpu_system_pct`, `cpu_iowait_pct` — percentages of wall-clock CPU time                                                      |
| Memory  | `mem_used_bytes`, `mem_total_bytes`, `swap_used_bytes`, `swap_total_bytes`                                                                   |
| Load    | `load1`, `load5`, `load15`                                                                                                                   |
| Disk    | `disk_used_bytes`, `disk_total_bytes` (root), `disk_read_bps`, `disk_write_bps`, plus per-mount used/total for physical filesystems (max 20) |
| Network | `net_rx_bps`, `net_tx_bps`, plus per-interface rates for interfaces with traffic (max 10)                                                    |
| Host    | `uptime_seconds`, optional hostname, OS, architecture, agent version                                                                         |

`serveragent -metrics` prints this from the binary itself, so it cannot drift from what
the code does.

## What it does not collect

This list is as much a part of the product as the one above:

- **No process names, command lines, or arguments.** These routinely carry usernames, home
  directory paths, and secrets passed in `argv`.
- **No environment variables.**
- **No file contents or file names.**
- **No user accounts, logins, or sessions.**
- **No network peers, connections, or packet contents.**
- **No IP addresses are stored.** Ingest authenticates by server ID and key, so the source
  address adds nothing.

This is enforced on both ends. The server decodes the payload into a fixed schema with
`DisallowUnknownFields`, so a modified agent that tried to send process data would have
its request rejected outright rather than silently stored. See
[`protocol/`](protocol/) for the exact wire contract and
[`protocol_test.go`](protocol/protocol_test.go) for the tests that hold it in place.

## What it will never do

- **It never executes commands sent by the server.** The protocol has no such message.
- **It never downloads or runs code.** It checks whether a newer release exists and logs
  that fact; applying the update is your decision. An agent that could silently update
  itself would make one compromised release key a foothold on every host running it.

## Install

From your dashboard, add a server and copy the generated command:

```bash
curl -fsSL https://statuspage.me/install-server-agent.sh | sudo bash -s -- \
  --server-id <uuid> --ingest-key <key>
```

Piping a script into a root shell requires trusting the source. If you would rather not,
download [`install.sh`](install.sh) first and read it — it is written to be read.

The installer verifies the binary's SHA-256 against the published checksum and **refuses
to install if it cannot**. There is no soft-fail path.

It sets up:

- the binary at `/usr/local/bin/serveragent`
- config at `/etc/statuspage/serveragent.conf`, mode `0640`
- a spool directory at `/var/lib/statuspage/`
- a dedicated `statuspage-agent` system user with no login shell
- a systemd unit with `ProtectSystem=strict`, `ProtectHome=true`, `NoNewPrivileges=true`,
  an empty `CapabilityBoundingSet`, `SystemCallFilter=@system-service`,
  `MemoryDenyWriteExecute=true`, and 128M / 10% CPU caps

The agent needs to read system counters. It does not need to be root, and it is not.

## Configuration

`/etc/statuspage/serveragent.conf` is plain `KEY=value` — no YAML parser in a binary you
install on your own machines, and the same file works directly as a systemd
`EnvironmentFile`. Every key can be overridden by an environment variable of the same
name, which is what containers should do.

| Key                    | Default                                       | Notes                                                          |
|------------------------|-----------------------------------------------|----------------------------------------------------------------|
| `SP_ENDPOINT`          | —                                             | Required. Must be `https://` unless `SP_INSECURE=1`.           |
| `SP_SERVER_ID`         | —                                             | Required. From the dashboard.                                  |
| `SP_INGEST_KEY`        | —                                             | Required. Shown once at creation.                              |
| `SP_INTERVAL_SECONDS`  | `60`                                          | Between 10 and 900.                                            |
| `SP_SPOOL_PATH`        | `/var/lib/statuspage/serveragent-spool.jsonl` | Buffer for unsent samples.                                     |
| `SP_MAX_SPOOL_SAMPLES` | `2880`                                        | 48h at the default cadence.                                    |
| `SP_HOSTNAME`          | auto-detected                                 | **Set empty to send no hostname at all.**                      |
| `SP_INSECURE`          | `0`                                           | Local development only. The ingest key is a bearer credential. |

### About the hostname

Hostnames frequently embed a person's name — `leos-macbook-pro.local`,
`nikola-workstation` — which makes them personal data. Nothing here requires one:

```bash
SP_HOSTNAME=db-primary   # report a name you choose
SP_HOSTNAME=             # report none at all
```

It is editable from the dashboard afterwards, and is never rendered on a public status
page.

## Behaviour during an outage

If the endpoint is unreachable the agent keeps collecting and buffers to disk. When
connectivity returns the backlog is flushed oldest-first, and the buffer is cleared only
once the server confirms each batch was stored.

The buffer is **bounded** (`SP_MAX_SPOOL_SAMPLES`). At the cap the oldest samples are
dropped. That is deliberate: an agent that buffered without limit would, during a long
outage, fill the disk of the very host it was installed to monitor — turning a monitoring
tool into the cause of an incident.

Ingest is idempotent on `(server id, sample timestamp)`, so a retry after a timeout you
never saw the response to cannot double-write.

## Troubleshooting

```bash
systemctl status statuspage-serveragent
journalctl -u statuspage-serveragent -f

serveragent -metrics    # exactly what it collects
serveragent -dry-run    # a real sample from this host
serveragent -once       # collect and push a single sample, then exit
```

- **401 in the journal** — the key was rotated, or the server was deleted. The agent logs
  the rejection and drops the batch rather than retrying forever.
- **403** — the server is disabled in the dashboard.
- **Nothing at all** — check `SP_ENDPOINT` is reachable from the host.

## Building

```bash
make build      # current platform
make test       # go test -race ./...
make lint       # vet + gofmt check
make release    # linux/amd64 + linux/arm64 with SHA256SUMS into bin/dist/
make metrics    # print the collected-metric list
```

## The `protocol` package

[`protocol/`](protocol/) is the wire contract, imported by both this agent and the ingest
server. One definition means a field rename or a changed limit is a compile error rather
than a runtime failure on machines already in the field.

It depends only on the standard library, deliberately: the server imports it and must
never pull host-collection code into its build.

```go
import "github.com/hosted-status-page/hsp-server-agent/protocol"
```

## Licence

Apache 2.0 — see [LICENSE](LICENSE).
