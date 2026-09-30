package keywarden

import (
	"cmp"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
			w.WriteHeader(http.StatusAccepted)
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

	// The private scalar is sent as 0x big-endian hex.
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

// fakeOrchestrator serves an election whose status goes through statuses, one
// per poll, and its results.
func fakeOrchestrator(t *testing.T, statuses []ElectionResponse) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/elections/abcd":
			mu.Lock()
			el := statuses[min(polls, len(statuses)-1)]
			polls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(el)
		case "/elections/abcd/results":
			_ = json.NewEncoder(w).Encode(ResultsResponse{ElectionID: "abcd", Tally: []uint64{3, 1}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWaitForResults(t *testing.T) {
	finalizing := ElectionResponse{ID: "abcd", Status: StatusFinalizing}
	for _, tc := range []struct {
		name     string
		statuses []ElectionResponse
		timeout  time.Duration
		wantErr  string
	}{
		{name: "results", statuses: []ElectionResponse{finalizing, finalizing, {ID: "abcd", Status: StatusResults}}},
		{
			name:     "finalize failed",
			statuses: []ElectionResponse{finalizing, {ID: "abcd", Status: "decrypting", FinalizeError: "prover down"}},
			wantErr:  "finalize failed: prover down",
		},
		{name: "not finalizing", statuses: []ElectionResponse{{ID: "abcd", Status: "active"}}, wantErr: "election is active, not finalizing"},
		{name: "timeout", statuses: []ElectionResponse{finalizing}, timeout: 20 * time.Millisecond, wantErr: "no results .* within 20ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			cl := NewClient(fakeOrchestrator(t, tc.statuses).URL, "")
			timeout := cmp.Or(tc.timeout, time.Minute)
			res, err := cl.WaitForResults("abcd", time.Millisecond, timeout)
			if tc.wantErr != "" {
				c.Assert(err, qt.ErrorMatches, tc.wantErr)
				return
			}
			c.Assert(err, qt.IsNil)
			c.Assert(res.Tally, qt.DeepEquals, []uint64{3, 1})
		})
	}
}
