package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hosted-status-page/hsp-server-agent/protocol"
)

// DefaultConfigPath is where the installer writes the agent configuration.
//
// The format is plain KEY=value, not YAML, for two reasons: it keeps the agent free of
// a parser dependency (fewer bytes and less supply-chain surface in a binary customers
// install on their own hosts), and the same file can be handed directly to systemd as
// an EnvironmentFile.
const DefaultConfigPath = "/etc/statuspage/serveragent.conf"

// Config is the agent's runtime configuration.
type Config struct {
	// Endpoint is the base URL of the ingest API, e.g. https://statuspage.me
	Endpoint string

	// ServerID and IngestKey are issued when the host is registered in the dashboard.
	ServerID  string
	IngestKey string

	// Interval is the collection and push cadence.
	Interval time.Duration

	// SpoolPath is where unsent samples are buffered across restarts and outages.
	SpoolPath string

	// MaxSpoolSamples bounds the on-disk buffer so a long outage cannot fill the disk
	// of the very host being monitored.
	MaxSpoolSamples int

	// Hostname overrides the auto-detected hostname. Optional: hostnames often embed a
	// person's name, so operators who would rather not send theirs can set this to
	// anything, or to an empty value to send nothing at all.
	Hostname    string
	HostnameSet bool

	// Insecure allows a plain-HTTP endpoint. Only for local development.
	Insecure bool
}

// Defaults that apply when the config file and environment say nothing.
const (
	defaultInterval        = protocol.DefaultIntervalSeconds * time.Second
	defaultSpoolPath       = "/var/lib/statuspage/serveragent-spool.jsonl"
	defaultMaxSpoolSamples = 2880 // 48h at the default 60s cadence

	minInterval = 10 * time.Second
	maxInterval = 15 * time.Minute
)

// LoadConfig reads the config file if present, then lets environment variables override
// it, so a container can be configured without writing a file at all.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		Interval:        defaultInterval,
		SpoolPath:       defaultSpoolPath,
		MaxSpoolSamples: defaultMaxSpoolSamples,
	}

	fileValues, err := readKeyValueFile(path)
	if err != nil {
		return nil, err
	}

	get := func(key string) (string, bool) {
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		v, ok := fileValues[key]
		return v, ok
	}

	if v, ok := get("SP_ENDPOINT"); ok {
		cfg.Endpoint = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if v, ok := get("SP_SERVER_ID"); ok {
		cfg.ServerID = strings.TrimSpace(v)
	}
	if v, ok := get("SP_INGEST_KEY"); ok {
		cfg.IngestKey = strings.TrimSpace(v)
	}
	if v, ok := get("SP_SPOOL_PATH"); ok && strings.TrimSpace(v) != "" {
		cfg.SpoolPath = strings.TrimSpace(v)
	}
	if v, ok := get("SP_HOSTNAME"); ok {
		cfg.Hostname = strings.TrimSpace(v)
		cfg.HostnameSet = true
	}
	if v, ok := get("SP_INSECURE"); ok {
		cfg.Insecure = strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
	}
	if v, ok := get("SP_INTERVAL_SECONDS"); ok {
		secs, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("SP_INTERVAL_SECONDS: %w", err)
		}
		cfg.Interval = time.Duration(secs) * time.Second
	}
	if v, ok := get("SP_MAX_SPOOL_SAMPLES"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("SP_MAX_SPOOL_SAMPLES: %w", err)
		}
		cfg.MaxSpoolSamples = n
	}

	return cfg, nil
}

// validateEndpoint checks SP_ENDPOINT alone. It is shared with --update, which needs a
// trustworthy endpoint but has no use for the server ID or ingest key.
func (c *Config) validateEndpoint() error {
	if c.Endpoint == "" {
		return errors.New("SP_ENDPOINT is required")
	}
	if !strings.HasPrefix(c.Endpoint, "https://") {
		if !c.Insecure {
			// The ingest key is a bearer credential, so plain HTTP would put it on the
			// wire in clear text at every hop.
			return errors.New("SP_ENDPOINT must be https (set SP_INSECURE=1 to override for local development)")
		}
		if !strings.HasPrefix(c.Endpoint, "http://") {
			return errors.New("SP_ENDPOINT must start with http:// or https://")
		}
	}
	return nil
}

// Validate checks the configuration is usable before the agent starts collecting.
func (c *Config) Validate() error {
	if err := c.validateEndpoint(); err != nil {
		return err
	}
	if c.ServerID == "" {
		return errors.New("SP_SERVER_ID is required")
	}
	if c.IngestKey == "" {
		return errors.New("SP_INGEST_KEY is required")
	}
	if c.Interval < minInterval || c.Interval > maxInterval {
		return fmt.Errorf("SP_INTERVAL_SECONDS must be between %d and %d",
			int(minInterval.Seconds()), int(maxInterval.Seconds()))
	}
	if c.MaxSpoolSamples < 1 {
		return errors.New("SP_MAX_SPOOL_SAMPLES must be at least 1")
	}
	return nil
}

// readKeyValueFile parses a KEY=value file, ignoring blank lines and # comments. A
// missing file is not an error: the agent can be configured purely from the environment.
func readKeyValueFile(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: expected KEY=value", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// Tolerate quoted values, since the same file may be used as a shell env file.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		out[key] = value
	}
	return out, scanner.Err()
}

// Redacted returns the config with the ingest key masked, for logging.
func (c *Config) Redacted() string {
	key := "<unset>"
	if len(c.IngestKey) > 8 {
		key = c.IngestKey[:4] + "..." + c.IngestKey[len(c.IngestKey)-4:]
	} else if c.IngestKey != "" {
		key = "<set>"
	}
	return fmt.Sprintf("endpoint=%s server_id=%s key=%s interval=%s spool=%s max_spool=%d",
		c.Endpoint, c.ServerID, key, c.Interval, c.SpoolPath, c.MaxSpoolSamples)
}
