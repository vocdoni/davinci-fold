package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/golang-jwt/jwt/v4"
	bjjgnark "github.com/vocdoni/davinci-node/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-node/crypto/elgamal"
	"github.com/vocdoni/davinci-node/db"
	"github.com/vocdoni/davinci-node/db/metadb"

	"github.com/vocdoni/davinci-fold/orchestrator"
	"github.com/vocdoni/davinci-fold/storage"
)

const testJWTSecret = "test-secret"

// newTestAPI builds an API with a real in-memory engine and its router wired,
// but without starting an HTTP server, so handlers can be exercised in-process.
func newTestAPI(t *testing.T) *API {
	t.Helper()
	database, err := metadb.New(db.TypeInMem, "")
	qt.Assert(t, err, qt.IsNil)
	store := storage.New(database)
	engine, err := orchestrator.NewEngine(store, orchestrator.Options{BatchSize: 2})
	qt.Assert(t, err, qt.IsNil)
	t.Cleanup(engine.Stop)

	a := &API{
		engine:    engine,
		jwtSecret: []byte(testJWTSecret),
		batchSize: 64,
		foldEvery: 4,
	}
	a.initRouter()
	return a
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
	c := qt.New(t)
	a := newTestAPI(t)

	req := httptest.NewRequest(http.MethodPost, ElectionsEndpoint, nil)
	rec := httptest.NewRecorder()
	a.Router().ServeHTTP(rec, req)

	c.Assert(rec.Code, qt.Equals, http.StatusForbidden)
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
	pub, _, err := elgamal.GenerateKey(bjjgnark.New())
	qt.Assert(t, err, qt.IsNil)
	rx, ry := pub.(*bjjgnark.BJJ).Point()
	return &ElectionCreateRequest{
		ProcessID:  processID,
		BallotMode: "0x01",
		EncX:       "0x" + rx.Text(16),
		EncY:       "0x" + ry.Text(16),
		CensusRoot: "0x1234",
		BatchSize:  2,
		FoldEvery:  4,
		EndTime:    time.Now().Add(time.Hour),
	}
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
