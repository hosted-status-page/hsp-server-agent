package main

import (
	"testing"

	"github.com/hosted-status-page/hsp-server-agent/protocol"
	"github.com/shirou/gopsutil/v4/cpu"
)

func TestPopulateCPUPercentagesIncludesStealAndPreservesExistingBreakdown(t *testing.T) {
	var sample protocol.Sample
	populateCPUPercentages(&sample, cpuSnapshot(), cpu.TimesStat{
		User: 10, System: 20, Idle: 50, Iowait: 10, Steal: 10,
	})

	assertCPUPercent(t, "user", sample.CPUUserPct, 10)
	assertCPUPercent(t, "system", sample.CPUSystemPct, 20)
	assertCPUPercent(t, "iowait", sample.CPUIOWaitPct, 10)
	assertCPUPercent(t, "steal", sample.CPUStealPct, 10)
}

func TestPopulateCPUPercentagesKeepsMeasuredZeroSteal(t *testing.T) {
	var sample protocol.Sample
	populateCPUPercentages(&sample, cpuSnapshot(), cpu.TimesStat{
		User: 25, System: 25, Idle: 50,
	})

	assertCPUPercent(t, "steal", sample.CPUStealPct, 0)
}

func TestPopulateCPUPercentagesOmitsInvalidIntervalsAndCounterResets(t *testing.T) {
	t.Run("non-positive total delta", func(t *testing.T) {
		var sample protocol.Sample
		times := cpu.TimesStat{User: 10, Idle: 90, Steal: 5}
		populateCPUPercentages(&sample, cpuSnapshot(times), times)
		assertNoCPUPercentages(t, sample)
	})

	// A reset of any counter in the total poisons the shared denominator, so the whole
	// interval is dropped rather than only the metric whose counter went backwards.
	previous := cpu.TimesStat{
		User: 100, System: 100, Idle: 100, Nice: 100, Iowait: 100, Irq: 100, Softirq: 100, Steal: 100,
	}
	for name, reset := range map[string]func(*cpu.TimesStat){
		"user":    func(t *cpu.TimesStat) { t.User = 50 },
		"system":  func(t *cpu.TimesStat) { t.System = 50 },
		"idle":    func(t *cpu.TimesStat) { t.Idle = 50 },
		"nice":    func(t *cpu.TimesStat) { t.Nice = 50 },
		"iowait":  func(t *cpu.TimesStat) { t.Iowait = 50 },
		"irq":     func(t *cpu.TimesStat) { t.Irq = 50 },
		"softirq": func(t *cpu.TimesStat) { t.Softirq = 50 },
		"steal":   func(t *cpu.TimesStat) { t.Steal = 50 },
	} {
		t.Run(name+" counter reset", func(t *testing.T) {
			now := previous
			now.User += 100
			now.System += 100
			now.Idle += 100
			now.Nice += 100
			now.Iowait += 100
			now.Irq += 100
			now.Softirq += 100
			now.Steal += 100
			reset(&now)

			var sample protocol.Sample
			populateCPUPercentages(&sample, cpuSnapshot(previous), now)
			assertNoCPUPercentages(t, sample)
		})
	}
}

func TestPopulateCPUPercentagesBreakdownOverMonotonicInterval(t *testing.T) {
	previous := cpu.TimesStat{
		User: 100, System: 50, Idle: 500, Nice: 10, Iowait: 20, Irq: 5, Softirq: 5, Steal: 10,
	}
	now := cpu.TimesStat{
		User: 110, System: 60, Idle: 540, Nice: 15, Iowait: 25, Irq: 7, Softirq: 8, Steal: 12,
	}
	// Deltas: user 10, system 10, idle 40, nice 5, iowait 5, irq 2, softirq 3, steal 2.
	var sample protocol.Sample
	populateCPUPercentages(&sample, cpuSnapshot(previous), now)

	const total = 10 + 10 + 40 + 5 + 5 + 2 + 3 + 2
	assertCPUPercent(t, "user", sample.CPUUserPct, 15.0/total*100)
	assertCPUPercent(t, "system", sample.CPUSystemPct, 15.0/total*100)
	assertCPUPercent(t, "iowait", sample.CPUIOWaitPct, 5.0/total*100)
	assertCPUPercent(t, "steal", sample.CPUStealPct, 2.0/total*100)
}

func TestCollectCPUFirstSampleIsBaselineOnly(t *testing.T) {
	var sample protocol.Sample
	populateCPUPercentages(&sample, counterSnapshot{}, cpu.TimesStat{
		User: 10, System: 20, Idle: 50, Iowait: 10, Steal: 10,
	})
	assertNoCPUPercentages(t, sample)
}

func cpuSnapshot(times ...cpu.TimesStat) counterSnapshot {
	snapshot := counterSnapshot{cpuValid: true}
	if len(times) > 0 {
		snapshot.cpuTimes = times[0]
	}
	return snapshot
}

func assertCPUPercent(t *testing.T, name string, got *float32, want float32) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %v", name, want)
	}
	const epsilon = 0.0001
	if diff := *got - want; diff < -epsilon || diff > epsilon {
		t.Errorf("%s = %v, want %v", name, *got, want)
	}
}

func assertNoCPUPercentages(t *testing.T, sample protocol.Sample) {
	t.Helper()
	for _, metric := range []struct {
		name  string
		value *float32
	}{
		{"user", sample.CPUUserPct},
		{"system", sample.CPUSystemPct},
		{"iowait", sample.CPUIOWaitPct},
		{"steal", sample.CPUStealPct},
	} {
		if metric.value != nil {
			t.Errorf("%s = %v, want nil", metric.name, *metric.value)
		}
	}
}
