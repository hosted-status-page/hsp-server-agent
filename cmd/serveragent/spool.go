package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hosted-status-page/hsp-server-agent/protocol"
)

// Spool is a bounded, crash-durable buffer of unsent samples.
//
// It exists so a network outage does not punch a hole in the customer's metrics: the
// agent keeps collecting, and flushes the backlog when connectivity returns.
//
// It is bounded because the alternative is worse than losing data. An agent that
// buffered without limit would, during a long outage, fill the disk of the very host it
// was installed to monitor — turning a monitoring tool into the cause of an incident.
// When the cap is reached the OLDEST samples are dropped, since recent data is what
// anyone looks at first when service is restored.
//
// The on-disk format is JSON Lines: append-only writes, and a truncated final line from
// a power loss costs exactly one sample instead of the whole buffer.
type Spool struct {
	path    string
	maxSize int

	mu      sync.Mutex
	samples []protocol.Sample
}

// NewSpool loads any previously buffered samples from disk.
func NewSpool(path string, maxSize int) (*Spool, error) {
	s := &Spool{path: path, maxSize: maxSize}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the spool file, skipping any lines that fail to parse.
func (s *Spool) load() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Allow generously long lines: a sample with a full detail payload can exceed the
	// default 64KB scanner limit on a host with many mounts.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var sample protocol.Sample
		if err := json.Unmarshal(line, &sample); err != nil {
			// A partial trailing line after an unclean shutdown. Skip it.
			continue
		}
		s.samples = append(s.samples, sample)
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	s.trimLocked()
	return nil
}

// Add appends a sample, dropping the oldest if the buffer is full.
func (s *Spool) Add(sample protocol.Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, sample)
	s.trimLocked()
}

// Peek returns up to n of the oldest samples without removing them. Samples are only
// dropped once the server confirms it stored them.
func (s *Spool) Peek(n int) []protocol.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.samples) {
		n = len(s.samples)
	}
	out := make([]protocol.Sample, n)
	copy(out, s.samples[:n])
	return out
}

// Commit removes the n oldest samples after a successful push.
func (s *Spool) Commit(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.samples) {
		n = len(s.samples)
	}
	s.samples = s.samples[n:]
}

// Len reports how many samples are buffered.
func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.samples)
}

// trimLocked enforces the cap by discarding the oldest samples.
func (s *Spool) trimLocked() {
	if len(s.samples) <= s.maxSize {
		return
	}
	drop := len(s.samples) - s.maxSize
	s.samples = s.samples[drop:]
}

// Persist writes the buffer to disk atomically.
//
// The write goes to a temporary file which is then renamed over the target, so an
// interruption mid-write leaves the previous spool intact rather than a half-written
// file. Losing buffered metrics is acceptable; corrupting the buffer such that the
// agent cannot start is not.
func (s *Spool) Persist() error {
	s.mu.Lock()
	snapshot := make([]protocol.Sample, len(s.samples))
	copy(snapshot, s.samples)
	s.mu.Unlock()

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create spool directory: %w", err)
	}

	if len(snapshot) == 0 {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	tmp, err := os.CreateTemp(dir, ".serveragent-spool-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	writer := bufio.NewWriter(tmp)
	encoder := json.NewEncoder(writer)
	for i := range snapshot {
		if err := encoder.Encode(&snapshot[i]); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// The spool may contain nothing sensitive, but it describes the host's behaviour
	// over time, so keep it owner-readable rather than world-readable.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
