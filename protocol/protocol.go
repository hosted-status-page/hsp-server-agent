// Package protocol defines the wire format between the Server Agent and the ingest API.
//
// It exists so there is exactly one definition of that contract. The agent and the server
// live in separate repositories, and duplicating these types across them means a field
// rename or a changed limit becomes a runtime failure on machines that have already been
// installed. Here it is a compile error.
//
// The shared limits matter as much as the structs. An early version of the agent could
// produce a detail payload larger than the server's cap, and because the server rejects an
// oversize detail by rejecting the whole sample, hosts with many mounted filesystems lost
// every metric rather than just the breakdown. Two constants maintained in two places is
// exactly how that recurs.
//
// This package depends only on the standard library, deliberately: the server imports it,
// and must never pull host-collection code into its build.
package protocol

import (
	"encoding/json"
	"time"
)

// Headers the ingest API authenticates with.
//
// The credential travels in a header rather than the URL path so it is not captured by
// access logs and proxy logs along the request path.
const (
	// HeaderServerID identifies which registered host is reporting.
	HeaderServerID = "X-SP-Server-Id"
	// HeaderIngestKey carries the host's ingest key.
	HeaderIngestKey = "X-SP-Ingest-Key"
)

// Endpoint paths on the ingest API.
const (
	// PathIngest accepts a batch of samples.
	PathIngest = "/api/server-agent/ingest"
	// PathVersion reports the currently released agent version.
	PathVersion = "/api/server-agent/version"
)

// Limits enforced on both sides of the connection.
//
// The agent budget is deliberately below the server cap. The server rejects an oversize
// detail payload by rejecting the entire sample, so the agent trims to fit rather than
// letting the server decide — leaving headroom means a small encoding difference between
// versions cannot cost a host all of its metrics.
const (
	// MaxBatch is the most samples accepted in a single push.
	MaxBatch = 120

	// MaxDetailBytes is the server's hard cap on the encoded detail payload.
	MaxDetailBytes = 4096

	// MaxAgentDetailBudget is the agent's own, lower budget for the same payload.
	MaxAgentDetailBudget = 3072

	// MaxBodyBytes caps the whole request body.
	MaxBodyBytes = 512 * 1024

	// DefaultIntervalSeconds is the agent's default collection and push cadence.
	DefaultIntervalSeconds = 60

	// MaxOSPrettyBytes bounds IngestRequest.OSPretty. It is sent once per request, not
	// once per sample, so it does not enter the per-batch sizing assertions below —
	// 128 bytes against a 512KB body cap is noise.
	MaxOSPrettyBytes = 128
)

// Compile-time guarantee that the agent's detail budget stays strictly below the server's
// cap. If the two are ever brought level, this constant goes negative and the build fails
// here rather than in the field — where an oversize payload costs a host every metric in
// the sample, not just the breakdown.
const _ uint = MaxDetailBytes - MaxAgentDetailBudget - 1

// PerSampleOverheadBytes is the encoded size of one sample's scalar fields, excluding its
// detail payload. Measured against a worst case (every field populated, maximum-width
// values) by TestFullBatchFitsWithinBodyCap, then rounded up.
const PerSampleOverheadBytes = 600

// Compile-time guarantee that a full batch of maximally-detailed samples still fits inside
// the body cap. Exceeding MaxBodyBytes earns a 413, which a client is entitled to treat as
// permanent and discard, so a sizing mistake here is silent data loss rather than an error
// anyone sees.
const _ uint = MaxBodyBytes - (MaxBatch * (MaxAgentDetailBudget + PerSampleOverheadBytes)) - 1

// Sample is one wide row of host telemetry.
//
// The column set is fixed on purpose. That is the data-minimization mechanism as much as
// it is a performance decision: there is no field into which arbitrary customer data can
// arrive. The agent never collects process lists, command lines, or environment variables,
// all of which routinely carry usernames, home directory paths, and secrets in argv.
//
// Every metric is a pointer so that "not collected on this platform" stays distinct from
// "collected and measured zero" — swap on a host with no swap is not zero percent used,
// and a chart must render that as a gap rather than a trough.
type Sample struct {
	SampledAt time.Time `json:"sampled_at"`

	CPUUserPct   *float32 `json:"cpu_user_pct,omitempty"`
	CPUSystemPct *float32 `json:"cpu_system_pct,omitempty"`
	CPUIOWaitPct *float32 `json:"cpu_iowait_pct,omitempty"`
	CPUStealPct  *float32 `json:"cpu_steal_pct,omitempty"`

	MemUsedBytes   *int64 `json:"mem_used_bytes,omitempty"`
	MemTotalBytes  *int64 `json:"mem_total_bytes,omitempty"`
	SwapUsedBytes  *int64 `json:"swap_used_bytes,omitempty"`
	SwapTotalBytes *int64 `json:"swap_total_bytes,omitempty"`

	Load1  *float32 `json:"load1,omitempty"`
	Load5  *float32 `json:"load5,omitempty"`
	Load15 *float32 `json:"load15,omitempty"`

	DiskUsedBytes  *int64 `json:"disk_used_bytes,omitempty"`
	DiskTotalBytes *int64 `json:"disk_total_bytes,omitempty"`
	DiskReadBps    *int64 `json:"disk_read_bps,omitempty"`
	DiskWriteBps   *int64 `json:"disk_write_bps,omitempty"`

	NetRxBps *int64 `json:"net_rx_bps,omitempty"`
	NetTxBps *int64 `json:"net_tx_bps,omitempty"`

	UptimeSeconds *int64 `json:"uptime_seconds,omitempty"`

	// Detail carries only per-filesystem and per-interface breakdowns. The server
	// decodes it with DisallowUnknownFields, so anything outside Detail's shape is
	// rejected rather than stored.
	Detail json.RawMessage `json:"detail,omitempty"`
}

// Detail is the only shape the per-sample detail payload may take.
//
// Decoding into this struct rather than a free-form map is what keeps unexpected keys out
// of the database: a modified agent that tried to send process names or environment data
// has its request refused, not silently persisted.
type Detail struct {
	Filesystems []DetailFilesystem `json:"filesystems,omitempty"`
	Interfaces  []DetailInterface  `json:"interfaces,omitempty"`
}

// DetailFilesystem is usage for one mounted filesystem.
type DetailFilesystem struct {
	Mountpoint string `json:"mountpoint"`
	UsedBytes  int64  `json:"used_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

// DetailInterface is throughput for one network interface.
type DetailInterface struct {
	Name  string `json:"name"`
	RxBps int64  `json:"rx_bps"`
	TxBps int64  `json:"tx_bps"`
}

// IngestRequest is the body of a metrics push.
//
// The host facts travel with the batch rather than being configured server-side so a
// re-imaged host reports its own identity, but they are advisory: authentication comes
// from the ingest key, never from these fields.
//
// Hostname is optional. Hostnames frequently embed a person's name, which makes them
// personal data, and nothing in the protocol requires one — an operator may send an empty
// value and the host is identified by its server ID alone.
type IngestRequest struct {
	AgentVersion string `json:"agent_version"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Hostname     string `json:"hostname"`

	// OSPretty is a human-readable software description of the machine — a Linux
	// distribution name and version, or a macOS release name — for display only. It
	// describes the operating system, never a person, and it is capped at
	// MaxOSPrettyBytes and truncated by the agent before it ever reaches the wire.
	// Optional: an agent that cannot determine it sends nothing, same as Hostname.
	OSPretty string `json:"os_pretty,omitempty"`

	Samples []Sample `json:"samples"`
}

// IngestResponse tells the agent what happened so it can trim its local buffer.
//
// Stored may be less than Accepted without anything being wrong: ingest is idempotent on
// (server id, sample timestamp), so a replayed batch is accepted and stores nothing.
type IngestResponse struct {
	Accepted int `json:"accepted"`
	Stored   int `json:"stored"`
	Rejected int `json:"rejected"`
}

// VersionResponse reports the currently released agent version.
//
// It is advisory. The agent logs that an update exists; it never downloads or executes
// anything, because self-updating code on someone else's production host turns one
// compromised release key into a foothold on every install.
type VersionResponse struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url,omitempty"`
	Checksum    string `json:"checksum,omitempty"`
}
