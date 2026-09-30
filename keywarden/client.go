// Package keywarden is the client side of the key-abstracted finalize: the
// orchestrator holds only the election encryption public key, publishes the
// encrypted results ciphertext at election end, receives the decryption key
// back from the keywarden and finalizes in the background, which the
// keywarden follows through the election's status. cmd/test-keywarden is
// built on this client.
package keywarden

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"
)

// EncryptedResultsResponse is the orchestrator's published results ciphertext:
// one ElGamal ciphertext per ballot field, each as four little-endian hex
// Twisted-Edwards coordinates.
type EncryptedResultsResponse struct {
	ElectionID string   `json:"election_id"`
	Ciphertext []string `json:"ciphertext"`
}

// DecryptionKeyRequest carries the key the keywarden returns for an election's
// results: the raw ElGamal private scalar as 0x big-endian hex.
type DecryptionKeyRequest struct {
	Key string `json:"key"`
}

// Election statuses the keywarden follows.
const (
	StatusFinalizing = "finalizing"
	StatusResults    = "results"
)

// ElectionResponse is the part of the orchestrator's election view the
// keywarden follows: after the key is submitted the election is finalizing,
// then it has results, or it is decrypting again with FinalizeError set.
type ElectionResponse struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	FinalizeError string `json:"finalizeError,omitempty"`
}

// ResultsResponse is an election's final tally and the four Solidity-ready
// PLONK fields.
type ResultsResponse struct {
	ElectionID       string   `json:"electionID"`
	Tally            []uint64 `json:"tally"`
	ProgramVK        string   `json:"programVK"`
	RootCVadcopFinal string   `json:"rootCVadcopFinal"`
	PublicValues     string   `json:"publicValues"`
	ProofBytes       string   `json:"proofBytes"`
}

// maxPollErrs is how many consecutive failed polls end WaitForResults.
const maxPollErrs = 5

// Client talks to the orchestrator API from the keywarden's side: it fetches an
// election's published ciphertext, posts back the decryption key and follows
// the election to its results. The token authenticates as the keywarden role.
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

// NewClient builds a keywarden-side orchestrator client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		hc:      &http.Client{Timeout: 30 * time.Second},
	}
}

// EncryptedResults fetches an election's published results ciphertext.
func (c *Client) EncryptedResults(electionID string) (*EncryptedResultsResponse, error) {
	var out EncryptedResultsResponse
	if err := c.get(fmt.Sprintf("/elections/%s/encrypted-results", electionID), "encrypted-results", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Election fetches an election's status.
func (c *Client) Election(electionID string) (*ElectionResponse, error) {
	var out ElectionResponse
	if err := c.get("/elections/"+electionID, "election", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Results fetches an election's final tally and PLONK proof.
func (c *Client) Results(electionID string) (*ResultsResponse, error) {
	var out ResultsResponse
	if err := c.get(fmt.Sprintf("/elections/%s/results", electionID), "results", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitForResults polls an election every interval until it has results,
// which it returns. It fails when the finalize failed, when the election is
// not finalizing, or when timeout elapses first.
func (c *Client) WaitForResults(electionID string, interval, timeout time.Duration) (*ResultsResponse, error) {
	deadline := time.Now().Add(timeout)
	pollErrs := 0
	for {
		el, err := c.Election(electionID)
		switch {
		case err != nil:
			if pollErrs++; pollErrs >= maxPollErrs {
				return nil, err
			}
		case el.Status == StatusResults:
			return c.Results(electionID)
		case el.FinalizeError != "":
			return nil, fmt.Errorf("finalize failed: %s", el.FinalizeError)
		case el.Status != StatusFinalizing:
			return nil, fmt.Errorf("election is %s, not finalizing", el.Status)
		default:
			pollErrs = 0
		}
		if time.Now().Add(interval).After(deadline) {
			return nil, fmt.Errorf("no results for election %s within %s", electionID, timeout)
		}
		time.Sleep(interval)
	}
}

// SubmitDecryptionKey posts the decryption key (the raw private scalar),
// which the orchestrator checks before starting the finalize in the
// background. Follow it with WaitForResults.
func (c *Client) SubmitDecryptionKey(electionID string, key *big.Int) error {
	body, err := json.Marshal(&DecryptionKeyRequest{Key: "0x" + key.Text(16)})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/elections/%s/decryption-key", c.baseURL, electionID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return httpErr("decryption-key", resp)
	}
	return nil
}

// get fetches path and decodes the JSON response into out.
func (c *Client) get(path, op string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return httpErr(op, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", op, err)
	}
	return nil
}

func (c *Client) auth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func httpErr(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s: status %d: %s", op, resp.StatusCode, bytes.TrimSpace(b))
}
