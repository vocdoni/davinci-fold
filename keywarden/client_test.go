package keywarden

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	qt "github.com/frankban/quicktest"
)

func TestEncryptedResultsAndSubmitKey(t *testing.T) {
	c := qt.New(t)

	const electionID = "abcd"
	const token = "kw-token"
	var gotKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.Check(r.Header.Get("Authorization"), qt.Equals, "Bearer "+token)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/elections/"+electionID+"/encrypted-results":
			_ = json.NewEncoder(w).Encode(EncryptedResultsResponse{
				ElectionID: electionID,
				Ciphertext: []string{"00", "01"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/elections/"+electionID+"/decryption-key":
			var req DecryptionKeyRequest
			c.Check(json.NewDecoder(r.Body).Decode(&req), qt.IsNil)
			gotKey = req.Key
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cl := NewClient(srv.URL, token)

	ct, err := cl.EncryptedResults(electionID)
	c.Assert(err, qt.IsNil)
	c.Assert(ct.ElectionID, qt.Equals, electionID)
	c.Assert(ct.Ciphertext, qt.DeepEquals, []string{"00", "01"})

	// v1 releases the private scalar as 0x big-endian hex.
	c.Assert(cl.SubmitDecryptionKey(electionID, big.NewInt(0xdead)), qt.IsNil)
	c.Assert(gotKey, qt.Equals, "0xdead")
}

func TestEncryptedResultsHTTPError(t *testing.T) {
	c := qt.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	cl := NewClient(srv.URL, "")
	_, err := cl.EncryptedResults("x")
	c.Assert(err, qt.Not(qt.IsNil))
}
