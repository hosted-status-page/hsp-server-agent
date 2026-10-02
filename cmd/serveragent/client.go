package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hosted-status-page/hsp-server-agent/protocol"
)

// Client pushes batches to the ingest API.
type Client struct {
	endpoint  string
	serverID  string
	ingestKey string
	http      *http.Client
}

// NewClient builds an ingest client with a bounded timeout, so a hung connection cannot
// stall the collection loop indefinitely.
func NewClient(cfg *Config) *Client {
	return &Client{
		endpoint:  cfg.Endpoint,
		serverID:  cfg.ServerID,
		ingestKey: cfg.IngestKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// errPermanent marks a failure that retrying will not fix, so the agent drops the batch
// instead of spinning on it forever.
type errPermanent struct{ msg string }

func (e *errPermanent) Error() string { return e.msg }

// IsPermanent reports whether an error should stop the agent retrying a batch.
func IsPermanent(err error) bool {
	var p *errPermanent
	return errors.As(err, &p)
}

// ErrBatchTooLarge means the encoded batch exceeds the server's body cap. The caller
// should split the batch and retry rather than drop it.
//
// This exists because the server answers an oversize body with 413, which Push otherwise
// classifies as permanent and discards. Turning that into a split converts a silent
// data-loss path into a non-event. The compile-time assertions in the protocol package
// make a full batch fit by construction, but they rely on an estimate of per-sample
// overhead; this is the guard for when that estimate is wrong.
var ErrBatchTooLarge = errors.New("encoded batch exceeds the server body limit")

// Push sends a batch of samples.
//
// Error classification is what keeps a broken agent from hammering the server: 4xx
// responses other than 429 are permanent (bad credentials, malformed payload, a deleted
// server) and the batch is discarded, while 429 and 5xx are transient and the caller
// backs off and retries the same batch.
func (c *Client) Push(ctx context.Context, req protocol.IngestRequest) (*protocol.IngestResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, &errPermanent{msg: fmt.Sprintf("encode batch: %v", err)}
	}

	if len(body) > protocol.MaxBodyBytes {
		return nil, ErrBatchTooLarge
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+protocol.PathIngest, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(protocol.HeaderServerID, c.serverID)
	httpReq.Header.Set(protocol.HeaderIngestKey, c.ingestKey)
	httpReq.Header.Set("User-Agent", "statuspage-serveragent/"+AgentVersion)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// Network-level failure: always worth retrying.
		return nil, err
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch {
	case resp.StatusCode == http.StatusOK:
		var out protocol.IngestResponse
		if err := json.Unmarshal(payload, &out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		return &out, nil

	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("rate limited by server")

	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, truncate(payload, 200))

	default:
		// 401, 403, 400, 413: retrying will not help. Report clearly so the operator
		// can see it in the journal rather than watching a silent retry loop.
		return nil, &errPermanent{
			msg: fmt.Sprintf("request rejected with %d: %s", resp.StatusCode, truncate(payload, 200)),
		}
	}
}

// FetchVersion asks the server which agent release is current. The running agent only
// reports an available update; it never downloads or executes anything on its own,
// because self-updating code on a customer's host is a supply-chain risk they did not
// sign up for by installing a metrics collector. Applying an update is a separate,
// operator-run command (--update), which uses this call to learn what "latest" is.
func (c *Client) FetchVersion(ctx context.Context) (*protocol.VersionResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint+protocol.PathVersion, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "statuspage-serveragent/"+AgentVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("version check returned %d", resp.StatusCode)
	}

	var out protocol.VersionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
