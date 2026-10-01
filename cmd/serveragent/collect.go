package main

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/hosted-status-page/hsp-server-agent/protocol"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

// counterSnapshot holds the cumulative counters needed to turn monotonic totals into
// per-second rates on the next collection.
type counterSnapshot struct {
	at time.Time

	cpuTimes cpu.TimesStat
	cpuValid bool

	diskReadBytes  uint64
	diskWriteBytes uint64
	diskValid      bool

	netRecvBytes uint64
	netSentBytes uint64
	netValid     bool

	perInterface map[string]net.IOCountersStat
}

// protocol.MaxAgentDetailBudget is the agent's own budget for the encoded detail payload.
//
// It sits deliberately below the server's 4096-byte cap, because the server rejects an
// oversize detail by rejecting the whole sample — a host with many mounts would
// otherwise lose every metric it ever collected, not merely the breakdown. The agent
// therefore trims to fit rather than letting the server decide.

// Collector turns successive counter readings into rate metrics.
type Collector struct {
	prev counterSnapshot

	// Entry caps bound the detail payload before the byte budget is applied.
	maxDetailFilesystems int
	maxDetailInterfaces  int
}

// NewCollector returns a collector with sane payload bounds.
func NewCollector() *Collector {
	return &Collector{
		maxDetailFilesystems: 20,
		maxDetailInterfaces:  10,
	}
}

// Collect gathers one sample.
//
// The first call after start cannot produce rate metrics (disk and network throughput
// need two readings to difference), so those fields are simply absent rather than zero.
// That distinction matters: the schema uses pointers so "not measured" never renders as
// a real trough on a chart.
func (c *Collector) Collect(ctx context.Context, now time.Time) protocol.Sample {
	s := protocol.Sample{SampledAt: now.UTC().Truncate(time.Second)}
	cur := counterSnapshot{at: now, perInterface: map[string]net.IOCountersStat{}}

	c.collectCPU(ctx, &s, &cur)
	c.collectMemory(ctx, &s)
	c.collectLoad(ctx, &s)
	c.collectDisk(ctx, &s, &cur)
	c.collectNetwork(ctx, &s, &cur)
	c.collectUptime(ctx, &s)
	c.attachDetail(ctx, &s, &cur)

	c.prev = cur
	return s
}

func (c *Collector) collectCPU(ctx context.Context, s *protocol.Sample, cur *counterSnapshot) {
	times, err := cpu.TimesWithContext(ctx, false)
	if err != nil || len(times) == 0 {
		return
	}
	cur.cpuTimes = times[0]
	cur.cpuValid = true

	populateCPUPercentages(s, c.prev, times[0])
}

// populateCPUPercentages turns two aggregate CPU counter snapshots into wall-clock
// percentages. A nil metric means the interval could not be measured; a measured zero
// remains a real zero pointer.
//
// If any counter that feeds the total moved backwards, the whole interval is discarded:
// the shared denominator would be wrong for every percentage, not only the one whose
// counter reset.
func populateCPUPercentages(s *protocol.Sample, prev counterSnapshot, now cpu.TimesStat) {
	if !prev.cpuValid {
		return
	}

	previous := prev.cpuTimes
	if cpuCountersMovedBackwards(previous, now) {
		return
	}

	// Total includes idle, so percentages are of wall-clock CPU time rather than of
	// busy time — a spike in iowait must not inflate the user percentage.
	deltaTotal := (now.User + now.System + now.Idle + now.Nice + now.Iowait +
		now.Irq + now.Softirq + now.Steal) -
		(previous.User + previous.System + previous.Idle + previous.Nice + previous.Iowait +
			previous.Irq + previous.Softirq + previous.Steal)
	if deltaTotal <= 0 {
		return
	}

	pct := func(nowV, prevV float64) *float32 {
		v := float32((nowV - prevV) / deltaTotal * 100)
		return &v
	}

	s.CPUUserPct = pct(now.User+now.Nice, previous.User+previous.Nice)
	s.CPUSystemPct = pct(now.System+now.Irq+now.Softirq, previous.System+previous.Irq+previous.Softirq)
	s.CPUIOWaitPct = pct(now.Iowait, previous.Iowait)
	s.CPUStealPct = pct(now.Steal, previous.Steal)
}

// cpuCountersMovedBackwards reports whether any counter that participates in the CPU
// total decreased between two snapshots, which happens when the counters reset (reboot,
// or a hypervisor adjusting accounting) and makes the interval meaningless.
func cpuCountersMovedBackwards(prev, now cpu.TimesStat) bool {
	return now.User < prev.User || now.System < prev.System || now.Idle < prev.Idle ||
		now.Nice < prev.Nice || now.Iowait < prev.Iowait || now.Irq < prev.Irq ||
		now.Softirq < prev.Softirq || now.Steal < prev.Steal
}

func (c *Collector) collectMemory(ctx context.Context, s *protocol.Sample) {
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil && vm != nil {
		s.MemUsedBytes = int64Ptr(int64(vm.Used))
		s.MemTotalBytes = int64Ptr(int64(vm.Total))
	}
	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil && sw != nil {
		s.SwapUsedBytes = int64Ptr(int64(sw.Used))
		s.SwapTotalBytes = int64Ptr(int64(sw.Total))
	}
}

func (c *Collector) collectLoad(ctx context.Context, s *protocol.Sample) {
	// Load average is a Unix concept; on platforms without it these stay absent.
	avg, err := load.AvgWithContext(ctx)
	if err != nil || avg == nil {
		return
	}
	s.Load1 = float32Ptr(float32(avg.Load1))
	s.Load5 = float32Ptr(float32(avg.Load5))
	s.Load15 = float32Ptr(float32(avg.Load15))
}

func (c *Collector) collectDisk(ctx context.Context, s *protocol.Sample, cur *counterSnapshot) {
	// Root filesystem usage is the headline number; per-mount detail rides along in
	// the detail payload.
	if usage, err := disk.UsageWithContext(ctx, "/"); err == nil && usage != nil {
		s.DiskUsedBytes = int64Ptr(int64(usage.Used))
		s.DiskTotalBytes = int64Ptr(int64(usage.Total))
	}

	counters, err := disk.IOCountersWithContext(ctx)
	if err != nil {
		return
	}
	var read, write uint64
	for _, ctr := range counters {
		read += ctr.ReadBytes
		write += ctr.WriteBytes
	}
	cur.diskReadBytes, cur.diskWriteBytes, cur.diskValid = read, write, true

	if !c.prev.diskValid {
		return
	}
	elapsed := cur.at.Sub(c.prev.at).Seconds()
	if elapsed <= 0 {
		return
	}
	s.DiskReadBps = rate(read, c.prev.diskReadBytes, elapsed)
	s.DiskWriteBps = rate(write, c.prev.diskWriteBytes, elapsed)
}

func (c *Collector) collectNetwork(ctx context.Context, s *protocol.Sample, cur *counterSnapshot) {
	totals, err := net.IOCountersWithContext(ctx, false)
	if err != nil || len(totals) == 0 {
		return
	}
	cur.netRecvBytes, cur.netSentBytes, cur.netValid = totals[0].BytesRecv, totals[0].BytesSent, true

	if perNIC, err := net.IOCountersWithContext(ctx, true); err == nil {
		for _, nic := range perNIC {
			cur.perInterface[nic.Name] = nic
		}
	}

	if !c.prev.netValid {
		return
	}
	elapsed := cur.at.Sub(c.prev.at).Seconds()
	if elapsed <= 0 {
		return
	}
	s.NetRxBps = rate(totals[0].BytesRecv, c.prev.netRecvBytes, elapsed)
	s.NetTxBps = rate(totals[0].BytesSent, c.prev.netSentBytes, elapsed)
}

func (c *Collector) collectUptime(ctx context.Context, s *protocol.Sample) {
	if up, err := host.UptimeWithContext(ctx); err == nil {
		s.UptimeSeconds = int64Ptr(int64(up))
	}
}

// attachDetail builds the per-filesystem and per-interface breakdown, then trims it to
// fit the byte budget.
func (c *Collector) attachDetail(ctx context.Context, s *protocol.Sample, cur *counterSnapshot) {
	detail := protocol.Detail{
		Filesystems: c.collectFilesystems(ctx),
		Interfaces:  c.collectInterfaces(cur),
	}

	if len(detail.Filesystems) == 0 && len(detail.Interfaces) == 0 {
		return
	}

	encoded, err := encodeDetailWithinBudget(detail, protocol.MaxAgentDetailBudget)
	if err != nil {
		return
	}
	s.Detail = encoded
}

// collectFilesystems returns per-mount usage for real filesystems.
//
// Mounts are deduplicated by device because several operating systems expose the same
// underlying volume at many paths — macOS mounts one APFS volume under a dozen
// /System/Volumes/* entries, and Linux bind mounts do the same. Reporting each of them
// would waste the payload budget restating one filesystem's figures.
func (c *Collector) collectFilesystems(ctx context.Context) []protocol.DetailFilesystem {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return nil
	}

	seenDevice := make(map[string]bool, len(parts))
	out := make([]protocol.DetailFilesystem, 0, len(parts))

	for _, p := range parts {
		if p.Device != "" {
			if seenDevice[p.Device] {
				continue
			}
			seenDevice[p.Device] = true
		}
		usage, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil || usage == nil || usage.Total == 0 {
			continue
		}
		out = append(out, protocol.DetailFilesystem{
			Mountpoint: p.Mountpoint,
			UsedBytes:  int64(usage.Used),
			TotalBytes: int64(usage.Total),
		})
	}

	// Largest first, so truncation drops the least interesting volumes.
	sort.Slice(out, func(i, j int) bool { return out[i].TotalBytes > out[j].TotalBytes })
	if len(out) > c.maxDetailFilesystems {
		out = out[:c.maxDetailFilesystems]
	}
	return out
}

// collectInterfaces returns per-interface throughput.
//
// Interfaces with no traffic in this window are skipped: a typical host carries a long
// tail of tunnels, bridges, and virtual interfaces that are permanently idle, and
// listing them crowds out the one interface that is actually moving data.
func (c *Collector) collectInterfaces(cur *counterSnapshot) []protocol.DetailInterface {
	if c.prev.perInterface == nil {
		return nil
	}
	elapsed := cur.at.Sub(c.prev.at).Seconds()
	if elapsed <= 0 {
		return nil
	}

	out := make([]protocol.DetailInterface, 0, len(cur.perInterface))
	for name, nic := range cur.perInterface {
		prev, ok := c.prev.perInterface[name]
		if !ok {
			continue
		}
		rx := rate(nic.BytesRecv, prev.BytesRecv, elapsed)
		tx := rate(nic.BytesSent, prev.BytesSent, elapsed)
		if rx == nil || tx == nil {
			continue
		}
		if *rx == 0 && *tx == 0 {
			continue
		}
		out = append(out, protocol.DetailInterface{Name: name, RxBps: *rx, TxBps: *tx})
	}

	// Busiest first, so truncation drops the quietest links.
	sort.Slice(out, func(i, j int) bool {
		return out[i].RxBps+out[i].TxBps > out[j].RxBps+out[j].TxBps
	})
	if len(out) > c.maxDetailInterfaces {
		out = out[:c.maxDetailInterfaces]
	}
	return out
}

// encodeDetailWithinBudget encodes the detail payload, dropping the least significant
// entries until it fits. Entries are already sorted most-significant-first, so this
// discards the smallest volumes and quietest interfaces before anything that matters.
func encodeDetailWithinBudget(detail protocol.Detail, budget int) ([]byte, error) {
	for {
		encoded, err := json.Marshal(detail)
		if err != nil {
			return nil, err
		}
		if len(encoded) <= budget {
			return encoded, nil
		}
		// Trim whichever list is longer, so one does not starve the other.
		switch {
		case len(detail.Filesystems) > len(detail.Interfaces) && len(detail.Filesystems) > 0:
			detail.Filesystems = detail.Filesystems[:len(detail.Filesystems)-1]
		case len(detail.Interfaces) > 0:
			detail.Interfaces = detail.Interfaces[:len(detail.Interfaces)-1]
		case len(detail.Filesystems) > 0:
			detail.Filesystems = detail.Filesystems[:len(detail.Filesystems)-1]
		default:
			// Nothing left to drop; an empty payload always fits.
			return json.Marshal(protocol.Detail{})
		}
	}
}

// rate converts two cumulative counter readings into bytes per second. A counter that
// went backwards means a reboot or an interface reset, which yields no reading at all
// rather than a bogus negative or a huge spike.
func rate(now, prev uint64, elapsedSeconds float64) *int64 {
	if now < prev || elapsedSeconds <= 0 {
		return nil
	}
	v := int64(float64(now-prev) / elapsedSeconds)
	return &v
}

func int64Ptr(v int64) *int64 { return &v }

func float32Ptr(v float32) *float32 { return &v }
