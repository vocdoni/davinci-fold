package api

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/golang-jwt/jwt/v4"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/storage"
	"github.com/vocdoni/davinci-fold/workers"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/circuits/ballotproof"
	bjjgnark "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/elgamal"
	spectestutil "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/spec/testutil"
)

const testJWTSecret = "test-secret"

// newTestAPI builds an API with a real in-memory engine and its router wired,
// but without starting an HTTP server, so handlers can be exercised in-process.
func newTestAPI(t *testing.T) *API {
	t.Helper()
	a, _ := newTestAPIWithStore(t)
	return a
}

// newTestAPIWithStore is newTestAPI returning the engine's storage too. The
// engine has a worker pool without workers: it can start a finalize, which
// fails for want of a prover.
func newTestAPIWithStore(t *testing.T) (*API, *storage.Storage) {
	t.Helper()
	database, err := metadb.New(db.TypeInMem, "")
	qt.Assert(t, err, qt.IsNil)
	store := storage.New(database)
	pool := workers.NewWorkerManager(nil)
	engine, err := orchestrator.NewEngine(store, orchestrator.Options{BatchSize: 2, FoldEvery: 4, Pool: pool})
	qt.Assert(t, err, qt.IsNil)
	t.Cleanup(engine.Stop)

	a := &API{
		engine:    engine,
		pool:      pool,
		jwtSecret: []byte(testJWTSecret),
		batchSize: 64,
		foldEvery: 4,
	}
	a.initRouter()
	return a, store
}

// do serves a request with an optional bearer token and JSON body.
func do(t *testing.T, a *API, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		qt.Assert(t, err, qt.IsNil)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	return rec
}

// newRequest builds a request with an optional Authorization header and body.
func newRequest(method, path, authHeader, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// serve runs req through the API's router.
func serve(a *API, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	return rec
}

// assertError checks rec is an error response with the HTTP status and code
// of want.
func assertError(t *testing.T, rec *httptest.ResponseRecorder, want Error) {
	t.Helper()
	qt.Assert(t, rec.Code, qt.Equals, want.HTTPstatus, qt.Commentf("body: %s", rec.Body.String()))
	var got struct {
		Code int `json:"code"`
	}
	qt.Assert(t, json.Unmarshal(rec.Body.Bytes(), &got), qt.IsNil)
	qt.Assert(t, got.Code, qt.Equals, want.Code, qt.Commentf("body: %s", rec.Body.String()))
}

// mintToken signs a JWT for the given role and subject using the test secret.
func mintToken(t *testing.T, role, subject string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"role": role,
		"sub":  subject,
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte(testJWTSecret))
	qt.Assert(t, err, qt.IsNil)
	return signed
}

func TestPing(t *testing.T) {
	c := qt.New(t)
	a := newTestAPI(t)

	req := httptest.NewRequest(http.MethodGet, PingEndpoint, nil)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)

	c.Assert(rec.Code, qt.Equals, http.StatusOK)
}

func TestInfo(t *testing.T) {
	c := qt.New(t)
	a := newTestAPI(t)

	req := httptest.NewRequest(http.MethodGet, InfoEndpoint, nil)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)

	c.Assert(rec.Code, qt.Equals, http.StatusOK)
	c.Assert(rec.Body.String(), qt.Contains, "batchSize")
}

// TestCreateElectionRequiresAuth verifies the admin route rejects unauthenticated
// requests before reaching the handler.
func TestCreateElectionRequiresAuth(t *testing.T) {
	a := newTestAPI(t)
	assertError(t, do(t, a, http.MethodPost, ElectionsEndpoint, "", nil), ErrInvalidToken)
}

// TestCreateElectionWrongRole verifies a keywarden token cannot create elections.
func TestCreateElectionWrongRole(t *testing.T) {
	c := qt.New(t)
	a := newTestAPI(t)

	req := httptest.NewRequest(http.MethodPost, ElectionsEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+mintToken(t, RoleKeywarden, "kw"))
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)

	c.Assert(rec.Code, qt.Equals, http.StatusForbidden)
}

// testElectionBody builds a valid create-election request bound to a fresh key.
func testElectionBody(t *testing.T, processID string) *ElectionCreateRequest {
	t.Helper()
	body, _ := testElectionBodyWithKey(t, processID)
	return body
}

// testElectionBodyWithKey is testElectionBody returning the private key too.
func testElectionBodyWithKey(t *testing.T, processID string) (*ElectionCreateRequest, *big.Int) {
	t.Helper()
	pub, priv, err := elgamal.GenerateKey(bjjgnark.New())
	qt.Assert(t, err, qt.IsNil)
	rx, ry := pub.(*bjjgnark.BJJ).Point()
	bm, err := spectestutil.FixedBallotMode().Pack()
	qt.Assert(t, err, qt.IsNil)
	return &ElectionCreateRequest{
		ProcessID:    processID,
		BallotMode:   "0x" + bm.Text(16),
		EncX:         "0x" + rx.Text(16),
		EncY:         "0x" + ry.Text(16),
		CensusOrigin: uint64(davinci.CensusOriginMerkle),
		CensusRoot:   "0x1234",
		VK:           json.RawMessage(ballotproof.CircomVerificationKey),
		BatchSize:    2,
		FoldEvery:    4,
		EndTime:      time.Now().Add(time.Hour),
	}, priv
}

// TestCreateAndGetElection drives the admin create path and the public read path.
func TestCreateAndGetElection(t *testing.T) {
	c := qt.New(t)
	a := newTestAPI(t)

	body, err := json.Marshal(testElectionBody(t, "0xabcdef"))
	c.Assert(err, qt.IsNil)
	req := httptest.NewRequest(http.MethodPost, ElectionsEndpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintToken(t, RoleAdmin, "admin"))
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	c.Assert(rec.Code, qt.Equals, http.StatusOK, qt.Commentf("body: %s", rec.Body.String()))

	var created ElectionResponse
	c.Assert(json.Unmarshal(rec.Body.Bytes(), &created), qt.IsNil)
	c.Assert(created.ID, qt.Equals, "abcdef")

	// Public read-back.
	req = httptest.NewRequest(http.MethodGet, "/elections/abcdef", nil)
	rec = httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	c.Assert(rec.Code, qt.Equals, http.StatusOK)

	// Listing surfaces the new election.
	req = httptest.NewRequest(http.MethodGet, ElectionsEndpoint, nil)
	rec = httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	c.Assert(rec.Code, qt.Equals, http.StatusOK)
	c.Assert(rec.Body.String(), qt.Contains, "abcdef")
}

// TestCreateElectionRejectsCSP verifies an election with a census origin
// ingest cannot verify is refused with a malformed-parameter error.
func TestCreateElectionRejectsCSP(t *testing.T) {
	c := qt.New(t)
	a := newTestAPI(t)

	el := testElectionBody(t, "0xc5b0")
	el.CensusOrigin = uint64(davinci.CensusOriginCSP)
	body, err := json.Marshal(el)
	c.Assert(err, qt.IsNil)
	req := httptest.NewRequest(http.MethodPost, ElectionsEndpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintToken(t, RoleAdmin, "admin"))
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	c.Assert(rec.Code, qt.Equals, http.StatusBadRequest)
	c.Assert(rec.Body.String(), qt.Contains, `"code":40003`)
	c.Assert(rec.Body.String(), qt.Contains, "census origin 4 (CSP) is not supported")
}

// TestEncryptedResultsGating verifies the keywarden endpoint refuses to serve
// ciphertext before the election reaches Decrypting.
func TestEncryptedResultsGating(t *testing.T) {
	c := qt.New(t)
	a := newTestAPI(t)

	body, err := json.Marshal(testElectionBody(t, "0xfeed"))
	c.Assert(err, qt.IsNil)
	req := httptest.NewRequest(http.MethodPost, ElectionsEndpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintToken(t, RoleAdmin, "admin"))
	a.Router().ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/elections/feed/encrypted-results", nil)
	req.Header.Set("Authorization", "Bearer "+mintToken(t, RoleKeywarden, "kw"))
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)
	// Not yet Decrypting: results-not-ready.
	c.Assert(rec.Code, qt.Equals, http.StatusConflict, qt.Commentf("body: %s", rec.Body.String()))
}

// TestListWorkersEmpty verifies the public worker list returns an empty array
// when no pool is configured.
func TestListWorkersEmpty(t *testing.T) {
	c := qt.New(t)
	a := newTestAPI(t)

	req := httptest.NewRequest(http.MethodGet, WorkersEndpoint, nil)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)

	c.Assert(rec.Code, qt.Equals, http.StatusOK)
	c.Assert(rec.Body.String(), qt.Contains, "workers")
}
