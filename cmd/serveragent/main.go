// Command serveragent collects host metrics and pushes them to a StatusPage.me account.
//
// This is the customer-installed agent. It is unrelated to the "agent" mode of
// cmd/server, which is the vendor-operated regional probe fleet running outbound checks.
//
// The agent is read-only by design: it collects a fixed list of numeric metrics and
// sends them over HTTPS. It executes no commands, accepts no instructions from the
// server, reads no file contents, and never downloads or runs code on its own. The one
// exception is `serveragent --update`, which an operator runs by hand to replace the
// binary with a checksum-verified release; nothing in the collection loop ever calls
// it. Run `serveragent -metrics` to print the complete list of what it collects.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/hosted-status-page/hsp-server-agent/protocol"
)

// AgentVersion is the build version, set at link time:
//
//	go build -ldflags "-X main.AgentVersion=1.2.3"
//
// The release version is the git tag. Every build (make build, make release, and the
// StatusPage.me server's publish step) stamps it, so the value here is only a placeholder
// for a plain `go build`; it is not a version anyone should edit.
var AgentVersion = "0.1.0"

// Push and retry behaviour.
const (
	// maxBatchSize matches the server's per-batch cap.
	maxBatchSize = protocol.MaxBatch

	// versionCheckInterval is how often the agent asks whether a newer release exists.
	versionCheckInterval = 24 * time.Hour

	// Backoff bounds for transient push failures.
	minBackoff = 30 * time.Second
	maxBackoff = 15 * time.Minute
)

func main() {
	var (
		configPath  = flag.String("config", DefaultConfigPath, "path to the configuration file")
		showVersion = flag.Bool("version", false, "print the agent version and exit")
		showMetrics = flag.Bool("metrics", false, "print the exact list of collected metrics and exit")
		oneShot     = flag.Bool("once", false, "collect and push a single sample, then exit")
		dryRun      = flag.Bool("dry-run", false, "collect a sample and print it without sending")
		update      = flag.Bool("update", false, "download, verify and install the latest release, then exit (does not restart the service)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("statuspage-serveragent %s (%s/%s)\n", AgentVersion, runtime.GOOS, runtime.GOARCH)
		return
	}
	if *showMetrics {
		printCollectedMetrics()
		return
	}
	if *update {
		os.Exit(runUpdate(*configPath, os.Stdout, os.Stderr))
	}

	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("serveragent: ")

	if *dryRun {
		runDryRun()
		return
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("configuration: %v", err)
	}
	log.Printf("starting %s (%s/%s) %s", AgentVersion, runtime.GOOS, runtime.GOARCH, cfg.Redacted())

	spool, err := NewSpool(cfg.SpoolPath, cfg.MaxSpoolSamples)
	if err != nil {
		log.Fatalf("spool: %v", err)
	}
	if n := spool.Len(); n > 0 {
		log.Printf("recovered %d buffered samples from a previous run", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	agent := &Agent{
		cfg:       cfg,
		collector: NewCollector(),
		client:    NewClient(cfg),
		spool:     spool,
		hostname:  resolveHostname(cfg),
		osPretty:  osPretty(),
	}

	if *oneShot {
		agent.collectOnce(ctx)
		if err := agent.flush(ctx); err != nil {
			log.Fatalf("push failed: %v", err)
		}
		return
	}

	agent.Run(ctx)
}

// Agent ties collection, buffering, and pushing together.
type Agent struct {
	cfg       *Config
	collector *Collector
	client    *Client
	spool     *Spool
	hostname  string
	osPretty  string

	// backoff is the current penalty after a transient push failure, and nextAttempt
	// is when pushing may resume. Both are only touched from the single Run loop.
	backoff     time.Duration
	nextAttempt time.Time
}

// Run drives the collection loop until the context is cancelled.
func (a *Agent) Run(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()

	versionTicker := time.NewTicker(versionCheckInterval)
	defer versionTicker.Stop()

	// Prime the rate counters. The first sample cannot carry throughput figures
	// because rates need two readings to difference, so take one and discard it.
	a.collector.Collect(ctx, time.Now())

	a.checkVersion(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down, persisting %d buffered samples", a.spool.Len())
			if err := a.spool.Persist(); err != nil {
				log.Printf("failed to persist spool: %v", err)
			}
			return

		case <-ticker.C:
			a.collectOnce(ctx)

			// While backing off, flush is a no-op, so nothing would write the spool to
			// disk until the penalty expires — at the 15-minute ceiling that leaves a
			// long window where an OOM kill or a hard power loss discards everything
			// collected since the last attempt. Persist here so an outage costs at most
			// one interval's worth of data.
			if time.Now().Before(a.nextAttempt) {
				if err := a.spool.Persist(); err != nil {
					log.Printf("failed to persist spool: %v", err)
				}
				continue
			}

			if err := a.flush(ctx); err != nil {
				log.Printf("push failed (%d buffered): %v", a.spool.Len(), err)
			}

		case <-versionTicker.C:
			a.checkVersion(ctx)
		}
	}
}

// collectOnce gathers a sample into the spool.
func (a *Agent) collectOnce(ctx context.Context) {
	sample := a.collector.Collect(ctx, time.Now())
	a.spool.Add(sample)
}

// flush pushes buffered samples, oldest first.
//
// Samples are only dropped from the buffer once the server has confirmed the write, so
// a failure anywhere in the path leaves the data intact for the next attempt.
func (a *Agent) flush(ctx context.Context) error {
	if a.spool.Len() == 0 {
		return nil
	}
	if time.Now().Before(a.nextAttempt) {
		// Still serving a penalty from a previous failure. Samples keep accumulating
		// in the spool meanwhile; nothing is lost by waiting.
		return nil
	}

	// Halve the batch and retry while the encoded body is over the server's cap. A
	// host with unusually many mounts can produce fatter samples than the protocol's
	// per-sample estimate assumes; splitting keeps those samples deliverable instead of
	// letting them earn a 413 and be discarded as permanently rejected.
	size := maxBatchSize
	var (
		batch []protocol.Sample
		resp  *protocol.IngestResponse
		err   error
	)
	for {
		batch = a.spool.Peek(size)
		if len(batch) == 0 {
			return nil
		}

		resp, err = a.client.Push(ctx, protocol.IngestRequest{
			AgentVersion: AgentVersion,
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			Hostname:     a.hostname,
			OSPretty:     a.osPretty,
			Samples:      batch,
		})
		if !errors.Is(err, ErrBatchTooLarge) {
			break
		}
		if size <= 1 {
			// A single sample that will not fit is undeliverable by any split.
			log.Printf("dropping 1 sample too large to send")
			a.spool.Commit(1)
			_ = a.spool.Persist()
			return nil
		}
		size /= 2
	}

	if err != nil {
		if IsPermanent(err) {
			// The server will never accept this batch. Drop it rather than retrying
			// forever, but make the reason loud: this is usually a revoked key or a
			// deleted host, and the operator needs to see it.
			log.Printf("dropping %d samples the server permanently rejected: %v", len(batch), err)
			a.spool.Commit(len(batch))
			_ = a.spool.Persist()
			return nil
		}
		a.penalise()
		_ = a.spool.Persist()
		return err
	}

	a.spool.Commit(len(batch))
	a.resetBackoff()

	if err := a.spool.Persist(); err != nil {
		log.Printf("failed to persist spool: %v", err)
	}

	if resp.Rejected > 0 {
		log.Printf("pushed %d samples, server stored %d and rejected %d",
			resp.Accepted+resp.Rejected, resp.Stored, resp.Rejected)
	}
	return nil
}

// penalise applies exponential backoff after a transient failure, doubling the wait up
// to a ceiling so a server outage produces a slow retry drumbeat rather than a flood.
func (a *Agent) penalise() {
	if a.backoff == 0 {
		a.backoff = minBackoff
	} else {
		a.backoff *= 2
		if a.backoff > maxBackoff {
			a.backoff = maxBackoff
		}
	}
	a.nextAttempt = time.Now().Add(a.backoff)
}

// resetBackoff clears the penalty after a successful push.
func (a *Agent) resetBackoff() {
	a.backoff = 0
	a.nextAttempt = time.Time{}
}

// checkVersion reports whether a newer agent release is available. It never downloads
// or installs anything; updating is the operator's decision (see runUpdate).
func (a *Agent) checkVersion(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	info, err := a.client.FetchVersion(checkCtx)
	if err != nil {
		return
	}
	if msg := versionCheckMessage(AgentVersion, info.Version); msg != "" {
		log.Print(msg)
	}
}

// versionCheckMessage turns the running and advertised versions into the line to log, or
// "" when there is nothing to say. Only a strictly newer advertised release counts as an
// update: an agent that is current or newer than the server expects (a canary, a rollback
// of the server) stays quiet, and versions that do not parse are reported as such rather
// than guessed at. The comparison is the one --update uses.
func versionCheckMessage(running, advertised string) string {
	if advertised == "" {
		return ""
	}
	newer, err := updateAvailable(running, advertised)
	if err != nil {
		return fmt.Sprintf("cannot compare agent versions (running %q, advertised %q): %v", running, advertised, err)
	}
	if !newer {
		return ""
	}
	return fmt.Sprintf("a newer agent is available (running %s, current %s); update with: sudo %s --update",
		running, advertised, installedBinaryPath)
}

// resolveHostname decides what hostname to report.
//
// An operator can set SP_HOSTNAME to an empty value to send nothing at all. That is a
// deliberate escape hatch: hostnames frequently embed a person's name, which makes them
// personal data, and nothing here requires one.
func resolveHostname(cfg *Config) string {
	if cfg.HostnameSet {
		return cfg.Hostname
	}
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// runDryRun collects one sample and prints it, so an operator can see exactly what
// would leave the host before configuring credentials.
func runDryRun() {
	ctx := context.Background()
	collector := NewCollector()

	// Two collections: the first primes the rate counters.
	collector.Collect(ctx, time.Now())
	time.Sleep(time.Second)
	sample := collector.Collect(ctx, time.Now())

	// Wrapped in the same envelope Push sends, not just the sample: agent_version, os,
	// arch, hostname, and os_pretty are real fields on every push and belong in "exactly
	// what would be sent", the same as the metrics themselves.
	req := protocol.IngestRequest{
		AgentVersion: AgentVersion,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Hostname:     resolveHostname(&Config{}),
		OSPretty:     osPretty(),
		Samples:      []protocol.Sample{sample},
	}

	encoded, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		log.Fatalf("encode request: %v", err)
	}
	fmt.Println("This is exactly what would be sent, and nothing else:")
	fmt.Println(string(encoded))
}

// printCollectedMetrics documents the full collection surface.
//
// This exists because asking someone to install a binary on their production host is
// asking for trust. The answer to "what does it actually send?" should be one command,
// not a support ticket.
func printCollectedMetrics() {
	fmt.Print(`statuspage-serveragent collects exactly the following, and nothing else:

CPU
  cpu_user_pct, cpu_system_pct, cpu_iowait_pct,
  cpu_steal_pct                                  percentages of wall-clock CPU time
  CPU steal is the percentage of CPU time the virtual machine was ready to run but the
  hypervisor was servicing another workload

Memory
  mem_used_bytes, mem_total_bytes
  swap_used_bytes, swap_total_bytes

Load
  load1, load5, load15                           Unix load averages

Disk
  disk_used_bytes, disk_total_bytes              root filesystem
  disk_read_bps, disk_write_bps                  throughput, all devices summed
  per-mount used/total bytes for physical filesystems (max 20)

Network
  net_rx_bps, net_tx_bps                         throughput, all interfaces summed
  per-interface rx/tx bytes per second (max 10)

Host
  uptime_seconds
  hostname, OS, architecture, agent version      hostname is optional, see SP_HOSTNAME
  OS distribution name and version                e.g. "Ubuntu 22.04.4 LTS", for display only

It does NOT collect:
  - process names, command lines, or arguments
  - environment variables
  - file contents or file names
  - user accounts, logins, or sessions
  - network peers, connections, or packet contents
  - anything that identifies a person

It also never executes commands sent by the server, and never downloads or runs code on
its own: only 'serveragent --update', which you run yourself, replaces the binary.
Run 'serveragent -dry-run' to print a real sample from this host.
`)
}
