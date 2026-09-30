package api

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/types"
)

// TestEngineErrorCodes checks each engine error maps to its API error, with
// the engine's message.
func TestEngineErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Error
	}{
		{orchestrator.ErrElectionNotFound, ErrElectionNotFound},
		{orchestrator.ErrNotAcceptingVotes, ErrElectionNotAcceptingVotes},
		{orchestrator.ErrVoteAlreadySubmitted, ErrVoteAlreadySubmitted},
		{orchestrator.ErrInvalidBallotProof, ErrInvalidBallotProof},
		{orchestrator.ErrInvalidSignature, ErrInvalidSignature},
		{orchestrator.ErrInvalidCensusProof, ErrInvalidCensusProof},
		{orchestrator.ErrMalformedVote, ErrMalformedBody},
		{orchestrator.ErrInvalidTransition, ErrInvalidStatusTransition},
		{orchestrator.ErrInvalidDecryptionKey, ErrInvalidDecryptionKey},
		{fmt.Errorf("persist vote: disk full"), ErrGenericInternalServerError},
	} {
		wrapped := fmt.Errorf("%w: detail", tc.err)
		got := engineError(wrapped)
		qt.Check(t, got.Code, qt.Equals, tc.want.Code, qt.Commentf("%v", tc.err))
		qt.Check(t, got.HTTPstatus, qt.Equals, tc.want.HTTPstatus, qt.Commentf("%v", tc.err))
		qt.Check(t, got.Error(), qt.Contains, wrapped.Error())
	}
	// The vote codes are the ones defined for them.
	for code, e := range map[int]Error{
		40008: ErrElectionNotAcceptingVotes, 40009: ErrInvalidBallotProof, 40010: ErrInvalidSignature,
		40011: ErrInvalidCensusProof, 40012: ErrVoteAlreadySubmitted,
	} {
		qt.Check(t, e.Code, qt.Equals, code)
	}
}

// createElection creates an election through the API and returns its ID and
// private key.
func createElection(t *testing.T, a *API, processID string) (string, *big.Int) {
	t.Helper()
	body, priv := testElectionBodyWithKey(t, processID)
	rec := do(t, a, http.MethodPost, ElectionsEndpoint, mintToken(t, RoleAdmin, "ops"), body)
	qt.Assert(t, rec.Code, qt.Equals, http.StatusOK, qt.Commentf("body: %s", rec.Body.String()))
	var el ElectionResponse
	qt.Assert(t, json.Unmarshal(rec.Body.Bytes(), &el), qt.IsNil)
	return el.ID, priv
}

// getElection reads an election through the API.
func getElection(t *testing.T, a *API, id string) *ElectionResponse {
	t.Helper()
	rec := do(t, a, http.MethodGet, "/elections/"+id, "", nil)
	qt.Assert(t, rec.Code, qt.Equals, http.StatusOK)
	var el ElectionResponse
	qt.Assert(t, json.Unmarshal(rec.Body.Bytes(), &el), qt.IsNil)
	return &el
}

// TestVoteRejectionCodes checks rejected votes get their reason's code.
func TestVoteRejectionCodes(t *testing.T) {
	a := newTestAPI(t)
	id, _ := createElection(t, a, "0x0c0de1")
	path := "/elections/" + id + "/votes"

	assertError(t, do(t, a, http.MethodPost, "/elections/beef/votes", "", &orchestrator.VoteSubmission{}), ErrElectionNotFound)
	assertError(t, serve(a, newRequest(http.MethodPost, path, "", "{")), ErrMalformedBody)
	rec := do(t, a, http.MethodPost, path, "", &orchestrator.VoteSubmission{Proof: []byte(`{}`), Sig: []byte(`{}`)})
	assertError(t, rec, ErrMalformedBody)
	qt.Assert(t, rec.Body.String(), qt.Contains, "malformed vote: address must be 20 bytes")
	assertError(t, serve(a, newRequest(http.MethodPost, path, "", `{"sig": {}}`)), ErrInvalidBallotProof)
	assertError(t, serve(a, newRequest(http.MethodPost, path, "", `{"proof": {}}`)), ErrInvalidSignature)

	rec = do(t, a, http.MethodPost, "/elections/"+id+"/status", mintToken(t, RoleAdmin, "ops"),
		&ElectionStatusRequest{Status: "paused"})
	qt.Assert(t, rec.Code, qt.Equals, http.StatusOK)
	assertError(t, do(t, a, http.MethodPost, path, "", &orchestrator.VoteSubmission{}), ErrElectionNotAcceptingVotes)
}

// TestElectionStatusEndpoint drives the organizer's status changes, allowed
// and refused.
func TestElectionStatusEndpoint(t *testing.T) {
	c := qt.New(t)
	a := newTestAPI(t)
	admin := mintToken(t, RoleAdmin, "ops")
	id, _ := createElection(t, a, "0x57a7e1")
	path := "/elections/" + id + "/status"
	set := func(token, status string) *ElectionResponse {
		rec := do(t, a, http.MethodPost, path, token, &ElectionStatusRequest{Status: status})
		c.Assert(rec.Code, qt.Equals, http.StatusOK, qt.Commentf("body: %s", rec.Body.String()))
		var el ElectionResponse
		c.Assert(json.Unmarshal(rec.Body.Bytes(), &el), qt.IsNil)
		return &el
	}

	assertError(t, do(t, a, http.MethodPost, path, "", &ElectionStatusRequest{Status: "paused"}), ErrInvalidToken)
	assertError(t, do(t, a, http.MethodPost, path, mintToken(t, RoleKeywarden, "kw"),
		&ElectionStatusRequest{Status: "paused"}), ErrUnauthorized)
	assertError(t, do(t, a, http.MethodPost, "/elections/beef/status", admin,
		&ElectionStatusRequest{Status: "paused"}), ErrElectionNotFound)
	for _, status := range []string{"", "results", "decrypting", "created", "PAUSED"} {
		assertError(t, do(t, a, http.MethodPost, path, admin, &ElectionStatusRequest{Status: status}), ErrMalformedParam)
	}

	c.Assert(set(admin, "paused").Status, qt.Equals, "paused")
	assertError(t, do(t, a, http.MethodPost, path, admin, &ElectionStatusRequest{Status: "paused"}), ErrInvalidStatusTransition)
	c.Assert(set(admin, "active").Status, qt.Equals, "active")
	c.Assert(set(admin, "ended").Status, qt.Equals, "ended")
	for _, status := range []string{"active", "paused", "ended", "canceled"} {
		assertError(t, do(t, a, http.MethodPost, path, admin, &ElectionStatusRequest{Status: status}), ErrInvalidStatusTransition)
	}

	other, _ := createElection(t, a, "0x57a7e2")
	path = "/elections/" + other + "/status"
	c.Assert(set(admin, "canceled").Status, qt.Equals, "canceled")
	assertError(t, do(t, a, http.MethodPost, path, admin, &ElectionStatusRequest{Status: "active"}), ErrInvalidStatusTransition)
	c.Assert(getElection(t, a, other).Status, qt.Equals, "canceled")
}

// TestDecryptionKeyEndpoint checks the key is checked at once, the finalize
// is started with a 202 and followed through the election's status, and a
// submission while finalizing is refused.
func TestDecryptionKeyEndpoint(t *testing.T) {
	c := qt.New(t)
	a, store := newTestAPIWithStore(t)
	keywarden := mintToken(t, RoleKeywarden, "kw")
	id, priv := createElection(t, a, "0xdec001")
	path := "/elections/" + id + "/decryption-key"
	elID := types.ElectionID{0xde, 0xc0, 0x01}
	submit := func(key string) *httptest.ResponseRecorder {
		return do(t, a, http.MethodPost, path, keywarden, &DecryptionKeyRequest{Key: key})
	}

	// Not decrypting yet.
	assertError(t, submit("0x"+priv.Text(16)), ErrInvalidStatusTransition)

	c.Assert(store.SetElectionStatus(elID, types.StatusDecrypting), qt.IsNil)
	assertError(t, submit("0xzz"), ErrMalformedParam)
	assertError(t, submit("0x"+new(big.Int).Add(priv, big.NewInt(1)).Text(16)), ErrInvalidDecryptionKey)
	c.Assert(getElection(t, a, id).Status, qt.Equals, "decrypting")

	rec := submit("0x" + priv.Text(16))
	c.Assert(rec.Code, qt.Equals, http.StatusAccepted, qt.Commentf("body: %s", rec.Body.String()))
	var accepted ElectionResponse
	c.Assert(json.Unmarshal(rec.Body.Bytes(), &accepted), qt.IsNil)
	c.Assert(accepted.ID, qt.Equals, id)

	// This engine has no prover: the finalize fails, and the election says so.
	deadline := time.Now().Add(5 * time.Second)
	el := getElection(t, a, id)
	for el.FinalizeError == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		el = getElection(t, a, id)
	}
	c.Assert(el.Status, qt.Equals, "decrypting")
	c.Assert(el.FinalizeError, qt.Equals, "prover unavailable")

	// While finalizing, a key is refused.
	c.Assert(store.SetElectionStatus(elID, types.StatusFinalizing), qt.IsNil)
	assertError(t, submit("0x"+priv.Text(16)), ErrInvalidStatusTransition)
}

// TestRemoveWorker checks an admin can take a worker out of the pool by ID.
func TestRemoveWorker(t *testing.T) {
	c := qt.New(t)
	a := newTestAPI(t)
	admin := mintToken(t, RoleAdmin, "ops")

	rec := do(t, a, http.MethodPost, WorkerRegisterEndpoint, admin, &WorkerRegisterRequest{Address: "http://10.0.0.5:8080"})
	c.Assert(rec.Code, qt.Equals, http.StatusOK)
	var list WorkersResponse
	c.Assert(json.Unmarshal(do(t, a, http.MethodGet, WorkersEndpoint, "", nil).Body.Bytes(), &list), qt.IsNil)
	c.Assert(len(list.Workers), qt.Equals, 1)
	workerID := list.Workers[0].ID
	c.Assert(workerID, qt.Not(qt.Equals), "")

	assertError(t, do(t, a, http.MethodDelete, "/workers/"+workerID, "", nil), ErrInvalidToken)
	c.Assert(do(t, a, http.MethodDelete, "/workers/"+workerID, admin, nil).Code, qt.Equals, http.StatusOK)
	c.Assert(json.Unmarshal(do(t, a, http.MethodGet, WorkersEndpoint, "", nil).Body.Bytes(), &list), qt.IsNil)
	c.Assert(len(list.Workers), qt.Equals, 0)
	assertError(t, do(t, a, http.MethodDelete, "/workers/"+workerID, admin, nil), ErrWorkerNotFound)
}

// TestElectionFoldEveryDefault checks an election created without foldEvery
// shows the cadence it uses.
func TestElectionFoldEveryDefault(t *testing.T) {
	a := newTestAPI(t)
	body := testElectionBody(t, "0xf01d")
	body.FoldEvery = 0
	rec := do(t, a, http.MethodPost, ElectionsEndpoint, mintToken(t, RoleAdmin, "ops"), body)
	qt.Assert(t, rec.Code, qt.Equals, http.StatusOK)
	var el ElectionResponse
	qt.Assert(t, json.Unmarshal(rec.Body.Bytes(), &el), qt.IsNil)
	qt.Assert(t, el.FoldEvery, qt.Equals, 4)
	qt.Assert(t, getElection(t, a, "f01d").FoldEvery, qt.Equals, 4)
}
