// Package keywarden is the client side of the key-abstracted finalize: the
// orchestrator holds only the voter encryption public key, publishes the
// encrypted results ciphertext at election end, and receives a decryption key
// back. v1 uses a local test keywarden (cmd/test-keywarden); the handshake is
// shaped for a future on-chain DKG, which will swap the raw scalar below for
// threshold decryption shares without changing the two-phase flow.
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
// 32 Twisted-Edwards little-endian hex coordinates (8 ElGamal ciphertexts).
type EncryptedResultsResponse struct {
	ElectionID string   `json:"election_id"`
	Ciphertext []string `json:"ciphertext"`
}

// DecryptionKeyRequest carries the key the keywarden returns for an election's
// results. For v1 this is the raw ElGamal private scalar as 0x big-endian hex;
// a future DKG variant carries threshold shares instead.
type DecryptionKeyRequest struct {
	Key string `json:"key"`
}

// Client talks to the orchestrator API from the keywarden's side: it fetches an
// election's published ciphertext and posts back the decryption key. The token
// authenticates as the keywarden role.
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
	url := fmt.Sprintf("%s/elections/%s/encrypted-results", c.baseURL, electionID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpErr("encrypted-results", resp)
	}
	var out EncryptedResultsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode encrypted-results: %w", err)
	}
	return &out, nil
}

// SubmitDecryptionKey posts the decryption key (v1: the raw private scalar),
// triggering the orchestrator's finalize.
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpErr("decryption-key", resp)
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
