package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosted-status-page/hsp-server-agent/protocol"
)

func TestReadKeyValueFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serveragent.conf")

	content := strings.Join([]string{
		"# a comment",
		"",
		"SP_ENDPOINT=https://example.test",
		"  SP_SERVER_ID = 11111111-2222-3333-4444-555555555555  ",
		`SP_INGEST_KEY="quoted-value"`,
		"SP_HOSTNAME='single-quoted'",
		"SP_INTERVAL_SECONDS=90",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	values, err := readKeyValueFile(path)
	if err != nil {
		t.Fatalf("readKeyValueFile: %v", err)
	}

	want := map[string]string{
		"SP_ENDPOINT":         "https://example.test",
		"SP_SERVER_ID":        "11111111-2222-3333-4444-555555555555",
		"SP_INGEST_KEY":       "quoted-value",
		"SP_HOSTNAME":         "single-quoted",
		"SP_INTERVAL_SECONDS": "90",
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s = %q, want %q", k, values[k], v)
		}
	}
}

func TestReadKeyValueFileMissingIsNotAnError(t *testing.T) {
	values, err := readKeyValueFile(filepath.Join(t.TempDir(), "absent.conf"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("got %d values from a missing file", len(values))
	}
}

// TestLoadConfigEnvOverridesFile covers container deployments, where the config file may
// not exist and everything arrives through the environment.
func TestLoadConfigEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serveragent.conf")
	if err := os.WriteFile(path, []byte("SP_ENDPOINT=https://from-file.test\nSP_SERVER_ID=file-id\nSP_INGEST_KEY=file-key\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("SP_ENDPOINT", "https://from-env.test")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Endpoint != "https://from-env.test" {
		t.Errorf("endpoint = %q, want the environment value", cfg.Endpoint)
	}
	if cfg.ServerID != "file-id" {
		t.Errorf("server id = %q, want the file value", cfg.ServerID)
	}
}

func TestConfigValidate(t *testing.T) {
	base := func() *Config {
		return &Config{
			Endpoint:        "https://example.test",
			ServerID:        "11111111-2222-3333-4444-555555555555",
			IngestKey:       strings.Repeat("a", 64),
			Interval:        defaultInterval,
			SpoolPath:       "/tmp/spool.jsonl",
			MaxSpoolSamples: 100,
		}
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("rejected a valid config: %v", err)
	}

	t.Run("rejects plain http by default", func(t *testing.T) {
		// The ingest key is a bearer credential; plain HTTP would expose it in transit.
		cfg := base()
		cfg.Endpoint = "http://example.test"
		if err := cfg.Validate(); err == nil {
			t.Error("accepted a plain-http endpoint without SP_INSECURE")
		}
	})

	t.Run("allows plain http when explicitly opted in", func(t *testing.T) {
		cfg := base()
		cfg.Endpoint = "http://localhost:18081"
		cfg.Insecure = true
		if err := cfg.Validate(); err != nil {
			t.Errorf("rejected an opted-in local endpoint: %v", err)
		}
	})

	t.Run("requires credentials", func(t *testing.T) {
		for _, mutate := range []func(*Config){
			func(c *Config) { c.Endpoint = "" },
			func(c *Config) { c.ServerID = "" },
			func(c *Config) { c.IngestKey = "" },
		} {
			cfg := base()
			mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("accepted a config missing a required field")
			}
		}
	})

	t.Run("bounds the interval", func(t *testing.T) {
		for _, d := range []time.Duration{time.Second, time.Hour} {
			cfg := base()
			cfg.Interval = d
			if err := cfg.Validate(); err == nil {
				t.Errorf("accepted an out-of-range interval %s", d)
			}
		}
	})
}

// TestConfigRedactedHidesKey guards against the ingest key reaching the journal, where
// it would outlive the process and be readable by anyone who can read logs.
func TestConfigRedactedHidesKey(t *testing.T) {
	key := strings.Repeat("s3cr3t", 10)
	cfg := &Config{
		Endpoint:  "https://example.test",
		ServerID:  "abc",
		IngestKey: key,
		Interval:  time.Minute,
	}
	out := cfg.Redacted()
	if strings.Contains(out, key) {
		t.Errorf("Redacted() leaked the full ingest key: %s", out)
	}
}

// TestSpoolDropsOldestWhenFull is the property that keeps the agent from filling the
// disk of the host it is monitoring during a long outage.
func TestSpoolDropsOldestWhenFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	spool, err := NewSpool(path, 3)
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		spool.Add(protocol.Sample{SampledAt: base.Add(time.Duration(i) * time.Minute)})
	}

	if got := spool.Len(); got != 3 {
		t.Fatalf("spool holds %d samples, want the 3-sample cap", got)
	}

	// The newest samples are the ones worth keeping.
	kept := spool.Peek(3)
	if !kept[0].SampledAt.Equal(base.Add(3 * time.Minute)) {
		t.Errorf("oldest kept sample is %s, want the 4th (older ones should be dropped)", kept[0].SampledAt)
	}
	if !kept[2].SampledAt.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("newest kept sample is %s, want the 6th", kept[2].SampledAt)
	}
}

// TestSpoolRoundTripsThroughDisk covers an agent restart with a backlog pending.
func TestSpoolRoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "spool.jsonl")

	spool, err := NewSpool(path, 100)
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		v := float32(i)
		spool.Add(protocol.Sample{SampledAt: base.Add(time.Duration(i) * time.Minute), CPUUserPct: &v})
	}
	if err := spool.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	reopened, err := NewSpool(path, 100)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.Len(); got != 4 {
		t.Fatalf("recovered %d samples, want 4", got)
	}
	recovered := reopened.Peek(4)
	if recovered[0].CPUUserPct == nil || *recovered[0].CPUUserPct != 0 {
		t.Error("sample values did not survive the round trip")
	}
	if !recovered[3].SampledAt.Equal(base.Add(3 * time.Minute)) {
		t.Error("sample order did not survive the round trip")
	}
}

// TestSpoolSurvivesTruncatedLine covers an unclean shutdown mid-write: one damaged
// sample must not cost the entire buffer.
func TestSpoolSurvivesTruncatedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.jsonl")

	good := `{"sampled_at":"2026-08-03T12:00:00Z"}` + "\n" +
		`{"sampled_at":"2026-08-03T12:01:00Z"}` + "\n" +
		`{"sampled_at":"2026-08-03T12:0` // truncated
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatalf("write spool: %v", err)
	}

	spool, err := NewSpool(path, 100)
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}
	if got := spool.Len(); got != 2 {
		t.Errorf("recovered %d samples, want 2 intact ones", got)
	}
}

func TestSpoolCommitRemovesOldest(t *testing.T) {
	spool, err := NewSpool(filepath.Join(t.TempDir(), "spool.jsonl"), 100)
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		spool.Add(protocol.Sample{SampledAt: base.Add(time.Duration(i) * time.Minute)})
	}

	spool.Commit(2)
	if got := spool.Len(); got != 3 {
		t.Fatalf("after committing 2 of 5, %d remain, want 3", got)
	}
	if !spool.Peek(1)[0].SampledAt.Equal(base.Add(2 * time.Minute)) {
		t.Error("Commit removed the wrong end of the buffer")
	}

	// Over-committing must not panic or go negative.
	spool.Commit(99)
	if got := spool.Len(); got != 0 {
		t.Errorf("over-commit left %d samples, want 0", got)
	}
}

// TestEncodeDetailWithinBudget is the guard for the bug found by running -dry-run on a
// developer Mac: 17 mounts and 10 interfaces produced a payload over the server's cap,
// and because the server rejects an oversize detail by rejecting the whole sample, that
// host would have lost every metric rather than just the breakdown.
func TestEncodeDetailWithinBudget(t *testing.T) {
	detail := protocol.Detail{}
	for i := 0; i < 60; i++ {
		detail.Filesystems = append(detail.Filesystems, protocol.DetailFilesystem{
			Mountpoint: "/Library/Developer/CoreSimulator/Volumes/very_long_volume_name_" + strings.Repeat("x", 20),
			UsedBytes:  1 << 40,
			TotalBytes: 1 << 41,
		})
	}
	for i := 0; i < 40; i++ {
		detail.Interfaces = append(detail.Interfaces, protocol.DetailInterface{
			Name: "interface-with-a-long-name", RxBps: 123456, TxBps: 654321,
		})
	}

	encoded, err := encodeDetailWithinBudget(detail, protocol.MaxAgentDetailBudget)
	if err != nil {
		t.Fatalf("encodeDetailWithinBudget: %v", err)
	}
	if len(encoded) > protocol.MaxAgentDetailBudget {
		t.Fatalf("encoded detail is %d bytes, over the %d budget", len(encoded), protocol.MaxAgentDetailBudget)
	}

	// The result must still be valid against the shape the server accepts.
	var parsed protocol.Detail
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		t.Fatalf("trimmed payload no longer matches the accepted shape: %v", err)
	}
	if len(parsed.Filesystems) == 0 && len(parsed.Interfaces) == 0 {
		t.Error("trimming discarded absolutely everything")
	}

	// Both lists should survive: trimming must not starve one to preserve the other.
	if len(parsed.Filesystems) == 0 {
		t.Error("all filesystems were trimmed away")
	}
	if len(parsed.Interfaces) == 0 {
		t.Error("all interfaces were trimmed away")
	}
}

func TestEncodeDetailWithinBudgetLeavesSmallPayloadsAlone(t *testing.T) {
	detail := protocol.Detail{
		Filesystems: []protocol.DetailFilesystem{{Mountpoint: "/", UsedBytes: 1, TotalBytes: 2}},
		Interfaces:  []protocol.DetailInterface{{Name: "eth0", RxBps: 10, TxBps: 20}},
	}
	encoded, err := encodeDetailWithinBudget(detail, protocol.MaxAgentDetailBudget)
	if err != nil {
		t.Fatalf("encodeDetailWithinBudget: %v", err)
	}
	var parsed protocol.Detail
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(parsed.Filesystems) != 1 || len(parsed.Interfaces) != 1 {
		t.Error("a payload already under budget was trimmed")
	}
}

// TestRateHandlesCounterReset covers a reboot or interface reset: a counter that goes
// backwards must produce no reading rather than a negative or absurdly large spike.
func TestRateHandlesCounterReset(t *testing.T) {
	if got := rate(50, 100, 60); got != nil {
		t.Errorf("counter reset produced %d, want no reading", *got)
	}
	if got := rate(100, 50, 0); got != nil {
		t.Error("zero elapsed time produced a reading")
	}
	got := rate(6000, 0, 60)
	if got == nil || *got != 100 {
		t.Errorf("rate = %v, want 100 bytes/sec", got)
	}
}

// TestResolveHostnameRespectsOptOut checks the privacy escape hatch: hostnames often
// embed a person's name, so an operator must be able to send none at all.
func TestResolveHostnameRespectsOptOut(t *testing.T) {
	if got := resolveHostname(&Config{HostnameSet: true, Hostname: ""}); got != "" {
		t.Errorf("explicit empty hostname produced %q, want empty", got)
	}
	if got := resolveHostname(&Config{HostnameSet: true, Hostname: "db-primary"}); got != "db-primary" {
		t.Errorf("override = %q, want db-primary", got)
	}
	if got := resolveHostname(&Config{}); got == "" {
		t.Error("with no override the agent should fall back to the real hostname")
	}
}

// TestAgentBackoffGrowsAndResets checks that a server outage produces a slow retry
// drumbeat rather than a flood, and that recovery clears the penalty immediately.
func TestAgentBackoffGrowsAndResets(t *testing.T) {
	a := &Agent{}

	a.penalise()
	first := a.backoff
	if first != minBackoff {
		t.Fatalf("first backoff = %s, want %s", first, minBackoff)
	}
	if a.nextAttempt.IsZero() {
		t.Fatal("penalise did not set nextAttempt")
	}

	a.penalise()
	if a.backoff != 2*minBackoff {
		t.Errorf("second backoff = %s, want %s", a.backoff, 2*minBackoff)
	}

	for i := 0; i < 20; i++ {
		a.penalise()
	}
	if a.backoff > maxBackoff {
		t.Errorf("backoff grew to %s, past the %s ceiling", a.backoff, maxBackoff)
	}

	a.resetBackoff()
	if a.backoff != 0 || !a.nextAttempt.IsZero() {
		t.Error("resetBackoff did not clear the penalty")
	}
}

// TestSampleOmitsUnmeasuredFields checks the pointer-based schema: a metric that could
// not be measured must be absent from the payload, never serialised as a real zero that
// would draw a false trough on a chart.
func TestSampleOmitsUnmeasuredFields(t *testing.T) {
	s := protocol.Sample{SampledAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)}
	encoded, err := json.Marshal(s)
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
	s.CPUUserPct = &zero
	encoded, _ = json.Marshal(s)
	if !strings.Contains(string(encoded), "cpu_user_pct") {
		t.Error("a genuinely measured zero was omitted from the payload")
	}
}

// TestPushRejectsOversizeBodyDistinctly checks that an oversize batch is reported as
// ErrBatchTooLarge rather than as a generic or permanent failure.
//
// The distinction is the whole point: Push classifies 4xx responses as permanent and the
// caller discards those batches, so an oversize body that reached the server would earn a
// 413 and take the samples with it. Catching it client-side turns that into a split.
func TestPushRejectsOversizeBodyDistinctly(t *testing.T) {
	client := NewClient(&Config{
		Endpoint:  "http://127.0.0.1:1", // never dialled; the size check comes first
		ServerID:  "s",
		IngestKey: "k",
	})

	big := make([]byte, protocol.MaxBodyBytes)
	for i := range big {
		big[i] = 'x'
	}

	_, err := client.Push(context.Background(), protocol.IngestRequest{
		AgentVersion: string(big),
		Samples:      []protocol.Sample{{SampledAt: time.Now().UTC()}},
	})
	if !errors.Is(err, ErrBatchTooLarge) {
		t.Fatalf("err = %v, want ErrBatchTooLarge", err)
	}
	if IsPermanent(err) {
		t.Error("ErrBatchTooLarge must not be permanent, or the caller will discard the batch")
	}
}

// TestPushAcceptsWorstCaseBatch is the counterpart: a full batch of maximally-detailed
// samples must pass the size guard, or the agent would split batches forever on a busy
// host and never make progress.
func TestPushAcceptsWorstCaseBatch(t *testing.T) {
	var received int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		atomic.StoreInt64(&received, int64(len(body)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accepted":120,"stored":120,"rejected":0}`))
	}))
	defer srv.Close()

	client := NewClient(&Config{Endpoint: srv.URL, ServerID: "s", IngestKey: "k"})

	detail := protocol.Detail{}
	for {
		encoded, _ := json.Marshal(detail)
		if len(encoded) >= protocol.MaxAgentDetailBudget-120 {
			break
		}
		detail.Filesystems = append(detail.Filesystems, protocol.DetailFilesystem{
			Mountpoint: "/Library/Developer/CoreSimulator/Volumes/a_long_volume_name",
			UsedBytes:  1 << 40, TotalBytes: 1 << 41,
		})
	}
	rawDetail, _ := json.Marshal(detail)

	samples := make([]protocol.Sample, protocol.MaxBatch)
	for i := range samples {
		samples[i] = protocol.Sample{
			SampledAt: time.Now().UTC().Add(-time.Duration(i) * time.Minute),
			Detail:    rawDetail,
		}
	}

	resp, err := client.Push(context.Background(), protocol.IngestRequest{
		AgentVersion: AgentVersion, OS: "linux", Arch: "amd64",
		Hostname: "worst-case-host", Samples: samples,
	})
	if err != nil {
		t.Fatalf("worst-case batch was rejected: %v", err)
	}
	if resp.Stored != protocol.MaxBatch {
		t.Errorf("stored = %d, want %d", resp.Stored, protocol.MaxBatch)
	}
	if got := atomic.LoadInt64(&received); got > protocol.MaxBodyBytes {
		t.Errorf("sent %d bytes, over the %d cap", got, protocol.MaxBodyBytes)
	} else {
		t.Logf("worst-case batch sent: %d bytes", got)
	}
}
