package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestSampleJSONTagsAreStable pins the wire names.
//
// Sharing the Go type between the agent and the server prevents a field from being
// *removed* without a compile error, but not from being *renamed on the wire* — changing
// a json tag compiles fine on both sides and silently breaks every installed agent. This
// test is the guard for that, so the tag names are a deliberate decision rather than an
// accident of a rename.
func TestSampleJSONTagsAreStable(t *testing.T) {
	want := []string{
		"sampled_at",
		"cpu_user_pct", "cpu_system_pct", "cpu_iowait_pct", "cpu_steal_pct",
		"mem_used_bytes", "mem_total_bytes", "swap_used_bytes", "swap_total_bytes",
		"load1", "load5", "load15",
		"disk_used_bytes", "disk_total_bytes", "disk_read_bps", "disk_write_bps",
		"net_rx_bps", "net_tx_bps",
		"uptime_seconds", "detail",
	}

	f32 := func(v float32) *float32 { return &v }
	i64 := func(v int64) *int64 { return &v }

	// Populate every field so omitempty does not hide one.
	full := Sample{
		SampledAt:      time.Unix(0, 0).UTC(),
		CPUUserPct:     f32(1),
		CPUSystemPct:   f32(1),
		CPUIOWaitPct:   f32(1),
		CPUStealPct:    f32(1),
		MemUsedBytes:   i64(1),
		MemTotalBytes:  i64(1),
		SwapUsedBytes:  i64(1),
		SwapTotalBytes: i64(1),
		Load1:          f32(1),
		Load5:          f32(1),
		Load15:         f32(1),
		DiskUsedBytes:  i64(1),
		DiskTotalBytes: i64(1),
		DiskReadBps:    i64(1),
		DiskWriteBps:   i64(1),
		NetRxBps:       i64(1),
		NetTxBps:       i64(1),
		UptimeSeconds:  i64(1),
		Detail:         json.RawMessage(`{}`),
	}

	encoded, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range want {
		if _, ok := asMap[key]; !ok {
			t.Errorf("wire field %q is missing; renaming it breaks every installed agent", key)
		}
	}
	if len(asMap) != len(want) {
		t.Errorf("Sample serialises %d fields, expected %d — a field was added or removed: %v",
			len(asMap), len(want), asMap)
	}
}

// TestUnmeasuredFieldsAreOmitted checks the pointer-based schema: a metric that could not
// be measured must be absent, never serialised as a real zero that would draw a false
// trough on a chart.
func TestUnmeasuredFieldsAreOmitted(t *testing.T) {
	encoded, err := json.Marshal(Sample{SampledAt: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(asMap) != 1 {
		t.Errorf("an all-unmeasured sample serialised %d fields, want only sampled_at: %s",
			len(asMap), encoded)
	}

	zero := float32(0)
	encoded, _ = json.Marshal(Sample{SampledAt: time.Unix(0, 0).UTC(), CPUUserPct: &zero})
	if !strings.Contains(string(encoded), `"cpu_user_pct":0`) {
		t.Error("a genuinely measured zero was omitted from the payload")
	}

	encoded, _ = json.Marshal(Sample{SampledAt: time.Unix(0, 0).UTC(), CPUStealPct: &zero})
	if !strings.Contains(string(encoded), `"cpu_steal_pct":0`) {
		t.Error("a genuinely measured zero steal percentage was omitted from the payload")
	}
}

// TestDetailRejectsUnknownFields is the smuggling guard, asserted here rather than only on
// the server so the agent's own tests fail if the shape ever widens.
func TestDetailRejectsUnknownFields(t *testing.T) {
	valid := `{"filesystems":[{"mountpoint":"/","used_bytes":1,"total_bytes":2}],
	           "interfaces":[{"name":"eth0","rx_bps":1,"tx_bps":2}]}`

	decoder := json.NewDecoder(strings.NewReader(valid))
	decoder.DisallowUnknownFields()
	var d Detail
	if err := decoder.Decode(&d); err != nil {
		t.Fatalf("rejected a valid detail payload: %v", err)
	}

	for _, payload := range []string{
		`{"processes":[{"name":"sshd","cmdline":"/usr/sbin/sshd -D"}]}`,
		`{"env":{"AWS_SECRET_ACCESS_KEY":"leaked"}}`,
		`{"filesystems":[{"mountpoint":"/","used_bytes":1,"total_bytes":2,"owner":"someone"}]}`,
	} {
		dec := json.NewDecoder(strings.NewReader(payload))
		dec.DisallowUnknownFields()
		var out Detail
		if err := dec.Decode(&out); err == nil {
			t.Errorf("accepted a payload carrying data the agent must never collect: %s", payload)
		}
	}
}

// TestFullBatchFitsWithinBodyCap is the runtime counterpart to the compile-time
// assertion in protocol.go. The assertion uses an estimate of per-sample overhead; this
// builds a genuinely worst-case batch — every field populated, a detail payload at the
// agent's full budget — and measures the encoded result.
//
// It matters because exceeding the cap earns a 413, which a client may legitimately treat
// as permanent and discard, turning a size miscalculation into silent data loss.
func TestFullBatchFitsWithinBodyCap(t *testing.T) {
	f32 := func(v float32) *float32 { return &v }
	i64 := func(v int64) *int64 { return &v }

	// Build a detail payload right at the agent's budget.
	detail := Detail{}
	for len(mustJSON(t, detail)) < MaxAgentDetailBudget-120 {
		detail.Filesystems = append(detail.Filesystems, DetailFilesystem{
			Mountpoint: "/Library/Developer/CoreSimulator/Volumes/a_long_volume_name",
			UsedBytes:  1 << 40,
			TotalBytes: 1 << 41,
		})
	}
	rawDetail := mustJSON(t, detail)

	sample := Sample{
		SampledAt:      time.Now().UTC(),
		CPUUserPct:     f32(99.999),
		CPUSystemPct:   f32(99.999),
		CPUIOWaitPct:   f32(99.999),
		CPUStealPct:    f32(99.999),
		MemUsedBytes:   i64(1 << 62),
		MemTotalBytes:  i64(1 << 62),
		SwapUsedBytes:  i64(1 << 62),
		SwapTotalBytes: i64(1 << 62),
		Load1:          f32(9999.99),
		Load5:          f32(9999.99),
		Load15:         f32(9999.99),
		DiskUsedBytes:  i64(1 << 62),
		DiskTotalBytes: i64(1 << 62),
		DiskReadBps:    i64(1 << 62),
		DiskWriteBps:   i64(1 << 62),
		NetRxBps:       i64(1 << 62),
		NetTxBps:       i64(1 << 62),
		UptimeSeconds:  i64(1 << 62),
		Detail:         rawDetail,
	}

	req := IngestRequest{
		AgentVersion: "0.0.0-with-a-long-build-suffix",
		OS:           "linux",
		Arch:         "amd64",
		Hostname:     strings.Repeat("h", 253),
		OSPretty:     strings.Repeat("o", MaxOSPrettyBytes),
		Samples:      make([]Sample, MaxBatch),
	}
	for i := range req.Samples {
		req.Samples[i] = sample
	}

	encoded := mustJSON(t, req)
	if len(encoded) > MaxBodyBytes {
		t.Errorf("worst-case full batch encodes to %d bytes, over the %d cap; "+
			"lower MaxBatch or MaxAgentDetailBudget", len(encoded), MaxBodyBytes)
	}
	t.Logf("worst-case batch: %d bytes (%.0f%% of the %d cap)",
		len(encoded), 100*float64(len(encoded))/float64(MaxBodyBytes), MaxBodyBytes)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestIngestRequestRejectsUnknownFields mirrors the server's decoding, so a change here
// that would break ingest fails in this repo first.
func TestIngestRequestRejectsUnknownFields(t *testing.T) {
	valid := `{"agent_version":"0.1.0","os":"linux","arch":"amd64","hostname":"h",
	           "samples":[{"sampled_at":"2026-08-03T10:00:00Z"}]}`
	dec := json.NewDecoder(strings.NewReader(valid))
	dec.DisallowUnknownFields()
	var req IngestRequest
	if err := dec.Decode(&req); err != nil {
		t.Fatalf("rejected a valid request: %v", err)
	}
	if len(req.Samples) != 1 {
		t.Fatalf("decoded %d samples, want 1", len(req.Samples))
	}

	smuggled := `{"samples":[{"sampled_at":"2026-08-03T10:00:00Z"}],
	              "processes":[{"name":"sshd"}]}`
	dec = json.NewDecoder(strings.NewReader(smuggled))
	dec.DisallowUnknownFields()
	var bad IngestRequest
	if err := dec.Decode(&bad); err == nil {
		t.Error("accepted a request with an unknown top-level field")
	}
}
