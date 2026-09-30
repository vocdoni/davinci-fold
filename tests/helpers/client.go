package helpers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/vocdoni/davinci-fold/api"
	"github.com/vocdoni/davinci-fold/orchestrator"
)

// Client is a thin HTTP client for the davinci-fold API used by integration
// tests. Bearer tokens are passed per call so a single client can act as both
// admin and keywarden.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a client bound to baseURL.
func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 30 * time.Second}}
}

// WaitReady polls GET /ping until it answers 200 or the deadline elapses.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+api.PingEndpoint, nil)
		resp, err := c.http.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("ping did not become ready within %s", timeout)
}

// do issues a JSON request and decodes the response into out (if non-nil),
// returning an error for any non-2xx status.
func (c *Client) do(ctx context.Context, method, path, token string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// CreateElection creates an election (admin).
func (c *Client) CreateElection(ctx context.Context, token string, req *api.ElectionCreateRequest) (*api.ElectionResponse, error) {
	var out api.ElectionResponse
	if _, err := c.do(ctx, http.MethodPost, api.ElectionsEndpoint, token, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetElection reads a single election.
func (c *Client) GetElection(ctx context.Context, id string) (*api.ElectionResponse, error) {
	var out api.ElectionResponse
	if _, err := c.do(ctx, http.MethodGet, "/elections/"+id, "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SubmitVote ingests a self-authenticating vote.
func (c *Client) SubmitVote(ctx context.Context, id string, sub *orchestrator.VoteSubmission) (*api.VoteReceiptResponse, error) {
	var out api.VoteReceiptResponse
	if _, err := c.do(ctx, http.MethodPost, "/elections/"+id+"/votes", "", sub, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EncryptedResults fetches the published results ciphertext (keywarden).
func (c *Client) EncryptedResults(ctx context.Context, token, id string) (*api.EncryptedResultsResponse, int, error) {
	var out api.EncryptedResultsResponse
	code, err := c.do(ctx, http.MethodGet, "/elections/"+id+"/encrypted-results", token, nil, &out)
	if err != nil {
		return nil, code, err
	}
	return &out, code, nil
}

// SubmitDecryptionKey hands the decryption key to the orchestrator (keywarden),
// which starts the finalize and answers 202 with the election. The key is
// sent as 0x big-endian hex.
func (c *Client) SubmitDecryptionKey(ctx context.Context, token, id string, key *big.Int) (*api.ElectionResponse, int, error) {
	var out api.ElectionResponse
	req := &api.DecryptionKeyRequest{Key: "0x" + key.Text(16)}
	code, err := c.do(ctx, http.MethodPost, "/elections/"+id+"/decryption-key", token, req, &out)
	if err != nil {
		return nil, code, err
	}
	return &out, code, nil
}

// WaitResults polls an election until it has results, which it returns, or
// its finalize fails or the timeout elapses.
func (c *Client) WaitResults(ctx context.Context, id string, timeout time.Duration) (*api.ResultsResponse, error) {
	deadline := time.Now().Add(timeout)
	for {
		el, err := c.GetElection(ctx, id)
		if err != nil {
			return nil, err
		}
		switch {
		case el.Status == "results":
			return c.Results(ctx, id)
		case el.FinalizeError != "":
			return nil, fmt.Errorf("finalize failed: %s", el.FinalizeError)
		case time.Now().After(deadline):
			return nil, fmt.Errorf("no results within %s (status %s)", timeout, el.Status)
		}
		time.Sleep(2 * time.Second)
	}
}

// SetElectionStatus asks for an election status change (admin).
func (c *Client) SetElectionStatus(ctx context.Context, token, id, status string) (*api.ElectionResponse, int, error) {
	var out api.ElectionResponse
	code, err := c.do(ctx, http.MethodPost, "/elections/"+id+"/status", token, &api.ElectionStatusRequest{Status: status}, &out)
	if err != nil {
		return nil, code, err
	}
	return &out, code, nil
}

// Results reads the final tally and PLONK snark.
func (c *Client) Results(ctx context.Context, id string) (*api.ResultsResponse, error) {
	var out api.ResultsResponse
	if _, err := c.do(ctx, http.MethodGet, "/elections/"+id+"/results", "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegisterWorker adds a prover worker to the pool (admin).
func (c *Client) RegisterWorker(ctx context.Context, token, address, name string) error {
	_, err := c.do(ctx, http.MethodPost, api.WorkerRegisterEndpoint, token, &api.WorkerRegisterRequest{Address: address, Name: name}, nil)
	return err
}

// RemoveWorker takes a prover worker out of the pool (admin).
func (c *Client) RemoveWorker(ctx context.Context, token, workerID string) (int, error) {
	return c.do(ctx, http.MethodDelete, "/workers/"+workerID, token, nil, nil)
}

// ListWorkers returns the current pool.
func (c *Client) ListWorkers(ctx context.Context) (*api.WorkersResponse, error) {
	var out api.WorkersResponse
	if _, err := c.do(ctx, http.MethodGet, api.WorkersEndpoint, "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
