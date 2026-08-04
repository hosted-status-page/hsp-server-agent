# Privacy

Not legal advice. This document describes what the agent does; whether your use of it
satisfies a particular regulator is a question for your counsel.

## The short version

The agent collects a fixed list of numbers describing a machine. It does not collect
anything describing the people using that machine.

## Is host telemetry personal data?

Generally no. CPU percentages, memory bytes, load averages, and IO rates describe
hardware, not an identified or identifiable natural person, and so fall outside the GDPR's
material scope.

Two things can change that, and both are handled deliberately.

### Hostnames

A hostname frequently embeds a person's name — `leos-macbook-pro.local`,
`nikola-workstation`, `jsmith-dev`. Where it does, it identifies a natural person and is
personal data.

The agent therefore treats the hostname as optional, not incidental:

- it can be set to any value you choose (`SP_HOSTNAME=db-primary`)
- it can be suppressed entirely (`SP_HOSTNAME=`)
- it is editable and removable from the dashboard after the fact
- it is **never** rendered on a public status page

A host is identified to the server by its server ID, which is a random UUID. The hostname
is a convenience label, and the system works without it.

### The OS distribution string

The agent also reports a human-readable OS description — `PRETTY_NAME` from
`/etc/os-release` on Linux, or the product name and version from `sw_vers` on macOS —
so the dashboard can show "Ubuntu 22.04.4 LTS" instead of "linux/amd64".

This describes the machine's software, not a person, and unlike the hostname it carries
no naming convention that would put someone's identity in it. It is not exposed on any
public status page: `models.PublicServerMetric` on the server side is asserted by test
to carry no field beyond the metric values themselves, so this stays a dashboard-only
detail available to the account that owns the host.

### Free-form payloads

Process lists, command lines, and environment variables are where secrets and identities
actually live: usernames in paths, API keys passed in `argv`, tokens in the environment.

**The agent collects none of them**. This is enforced structurally rather than by policy:

- The agent's collection surface is a fixed struct of numeric fields
  ([`protocol.Sample`](../protocol/protocol.go)). There is no field into which arbitrary
  data can be placed.
- The per-sample `detail` payload is restricted to filesystem mountpoints and network
  interface names, and is re-encoded from a typed struct before sending, which discards
  anything not part of that shape.
- The **server** decodes with `DisallowUnknownFields`. A modified agent sending process
  data does not get it stored — it gets its request rejected with a 400.

The last point matters: the guarantee does not depend on running our build of the agent.

## What is never collected

- process names, command lines, or arguments
- environment variables
- file contents or file names
- user accounts, logins, or sessions
- network peers, connections, or packet contents
- IP addresses

## What is never done

- The agent never executes commands sent by the server. The protocol has no such message.
- The agent never downloads or runs code. It reports that a newer release exists; applying
  it is your decision.

## Data in transit

The endpoint must be `https://` unless explicitly overridden with `SP_INSECURE=1`, which
exists for local development only. The ingest key is a bearer credential and is sent in a
header rather than a URL path, so it is not captured by access logs or proxy logs along
the request path.

## Data at rest, locally

Unsent samples are buffered to `SP_SPOOL_PATH` (default
`/var/lib/statuspage/serveragent-spool.jsonl`), written `0600` in a directory owned by the
service account. The buffer holds the same numeric metrics described above and nothing
else. It is bounded, and is deleted when empty.

## Erasure

Deleting a server from the dashboard removes its stored metrics. Uninstalling
(`install.sh --uninstall`) removes the binary, config, local spool, and service account
from the host.

On the server side, raw samples live in time-partitioned tables that are dropped whole
after a few days, with aggregates retained per plan. Dropping a partition removes the data
outright rather than leaving deleted rows awaiting vacuum.

## Verifying any of this

```bash
serveragent -metrics    # the collection surface, printed by the binary itself
serveragent -dry-run    # a real sample from this host, printed and not sent
```

And the source is here.
