package api

import (
	"cmp"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/golang-jwt/jwt/v4"

	"github.com/vocdoni/davinci-fold/log"
	"github.com/vocdoni/davinci-fold/types"
)

// signToken signs claims with secret.
func signToken(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	qt.Assert(t, err, qt.IsNil)
	return signed
}

// TestJWTAuth checks the admin route answers 401 to every request without a
// valid, expiring token and 403 to a valid token of another role.
func TestJWTAuth(t *testing.T) {
	a := newTestAPI(t)
	hour := time.Now().Add(time.Hour).Unix()
	for _, tc := range []struct {
		name   string
		header string
		want   *Error // nil: the handler is reached
	}{
		{name: "no header", want: &ErrInvalidToken},
		{name: "not a bearer token", header: "Basic YWRtaW46YWRtaW4=", want: &ErrInvalidToken},
		{name: "empty bearer token", header: "Bearer ", want: &ErrInvalidToken},
		{name: "garbage", header: "Bearer not.a.jwt", want: &ErrInvalidToken},
		{
			name:   "other secret",
			header: "Bearer " + signToken(t, "other", jwt.MapClaims{"role": RoleAdmin, "exp": hour}),
			want:   &ErrInvalidToken,
		},
		{
			name:   "no exp",
			header: "Bearer " + signToken(t, testJWTSecret, jwt.MapClaims{"role": RoleAdmin, "sub": "ops"}),
			want:   &ErrInvalidToken,
		},
		{
			name:   "zero exp",
			header: "Bearer " + signToken(t, testJWTSecret, jwt.MapClaims{"role": RoleAdmin, "exp": 0}),
			want:   &ErrInvalidToken,
		},
		{
			name:   "string exp",
			header: "Bearer " + signToken(t, testJWTSecret, jwt.MapClaims{"role": RoleAdmin, "exp": "tomorrow"}),
			want:   &ErrInvalidToken,
		},
		{
			name: "expired",
			header: "Bearer " + signToken(t, testJWTSecret,
				jwt.MapClaims{"role": RoleAdmin, "exp": time.Now().Add(-time.Minute).Unix()}),
			want: &ErrInvalidToken,
		},
		{name: "other role", header: "Bearer " + mintToken(t, RoleKeywarden, "kw"), want: &ErrUnauthorized},
		{name: "no role", header: "Bearer " + signToken(t, testJWTSecret, jwt.MapClaims{"exp": hour}), want: &ErrUnauthorized},
		// The handler rejects the missing body.
		{name: "admin", header: "Bearer " + mintToken(t, RoleAdmin, "ops"), want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(a, newRequest(http.MethodPost, ElectionsEndpoint, tc.header, ""))
			if tc.want == nil {
				assertError(t, rec, ErrMalformedBody)
				return
			}
			assertError(t, rec, *tc.want)
		})
	}
}

// logToFile sends the log to a file at debug level for the rest of the test
// and returns its path.
func logToFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.log")
	log.Init(log.LogLevelDebug, path, nil)
	t.Cleanup(func() {
		log.Init(cmp.Or(os.Getenv("LOG_LEVEL"), log.LogLevelError), "stderr", nil)
		DisabledLogging = false
	})
	return path
}

// readLog returns what was logged to path after offset, and the new offset.
func readLog(t *testing.T, path string, offset int) (string, int) {
	t.Helper()
	b, err := os.ReadFile(path)
	qt.Assert(t, err, qt.IsNil)
	return string(b[offset:]), len(b)
}

// TestLogRedaction checks debug logging never writes the bodies of the
// decryption-key route, nor any Authorization header, while it logs the
// bodies of the other routes.
func TestLogRedaction(t *testing.T) {
	c := qt.New(t)
	logPath := logToFile(t)
	a, store := newTestAPIWithStore(t)
	admin := mintToken(t, RoleAdmin, "ops")
	keywarden := mintToken(t, RoleKeywarden, "kw")

	body, priv := testElectionBodyWithKey(t, "0x5eed01")
	rec := do(t, a, http.MethodPost, ElectionsEndpoint, admin, body)
	c.Assert(rec.Code, qt.Equals, http.StatusOK, qt.Commentf("body: %s", rec.Body.String()))
	logged, offset := readLog(t, logPath, 0)
	c.Assert(logged, qt.Contains, "processID:0x5eed01") // the request body
	c.Assert(logged, qt.Contains, "id:5eed01")          // the response body

	c.Assert(store.SetElectionStatus(types.ElectionID{0x5e, 0xed, 0x01}, types.StatusDecrypting), qt.IsNil)
	key := "0x" + priv.Text(16)
	path := "/elections/5eed01/decryption-key"
	rec = do(t, a, http.MethodPost, path, keywarden, &DecryptionKeyRequest{Key: key})
	c.Assert(rec.Code, qt.Equals, http.StatusAccepted, qt.Commentf("body: %s", rec.Body.String()))
	logged, _ = readLog(t, logPath, offset)
	c.Assert(logged, qt.Contains, "/elections/5eed01/decryption-key")
	c.Assert(logged, qt.Not(qt.Contains), priv.Text(16))
	c.Assert(logged, qt.Not(qt.Contains), "data=")

	// Nor are the error messages of that route, while other routes' are.
	_, offset = readLog(t, logPath, 0)
	assertError(t, do(t, a, http.MethodPost, path, keywarden, &DecryptionKeyRequest{Key: "0xzz"}), ErrMalformedParam)
	logged, offset = readLog(t, logPath, offset)
	c.Assert(logged, qt.Contains, "/elections/5eed01/decryption-key")
	c.Assert(logged, qt.Not(qt.Contains), "API error response")
	assertError(t, do(t, a, http.MethodGet, "/elections/zz", "", nil), ErrMalformedParam)
	logged, _ = readLog(t, logPath, offset)
	c.Assert(logged, qt.Contains, "API error response")

	// A malformed body on that route is not logged either.
	_, offset = readLog(t, logPath, 0)
	serve(a, newRequest(http.MethodPost, "/elections/5eed01/decryption-key", "Bearer "+keywarden,
		`{"key": "0x`+priv.Text(16)))
	logged, _ = readLog(t, logPath, offset)
	c.Assert(logged, qt.Contains, "/elections/5eed01/decryption-key")
	c.Assert(logged, qt.Not(qt.Contains), priv.Text(16))

	// Nor is a body sent to a path no route matches.
	_, offset = readLog(t, logPath, 0)
	serve(a, newRequest(http.MethodPost, "/elections/5eed01/decryption-key/", "Bearer "+keywarden,
		`{"key": "0x`+priv.Text(16)+`"}`))
	logged, _ = readLog(t, logPath, offset)
	c.Assert(logged, qt.Contains, "/elections/5eed01/decryption-key/")
	c.Assert(logged, qt.Not(qt.Contains), priv.Text(16))

	all, _ := readLog(t, logPath, 0)
	for _, token := range []string{admin, keywarden} {
		c.Assert(all, qt.Not(qt.Contains), token)
		c.Assert(all, qt.Not(qt.Contains), strings.Split(token, ".")[2]) // the signature
	}

	// --log.disableAPI: nothing about requests is logged, errors included.
	DisabledLogging = true
	_, offset = readLog(t, logPath, 0)
	rec = do(t, a, http.MethodGet, "/elections/5eed01", "", nil)
	c.Assert(rec.Code, qt.Equals, http.StatusOK)
	assertError(t, do(t, a, http.MethodGet, "/elections/zz", "", nil), ErrMalformedParam)
	logged, _ = readLog(t, logPath, offset)
	c.Assert(logged, qt.Not(qt.Contains), "api request")
	c.Assert(logged, qt.Not(qt.Contains), "api response")
	c.Assert(logged, qt.Not(qt.Contains), "API error response")
}

// capBody is a request body over maxRequestBody: an unterminated JSON string
// that fails once read past maxRequestBody. read counts the bytes read.
type capBody struct{ read int }

func (b *capBody) Read(p []byte) (int, error) {
	const prefix = `{"proof":"`
	if b.read >= maxRequestBody {
		return 0, errors.New("body read past the cap")
	}
	n := min(len(p), maxRequestBody-b.read)
	for i := range n {
		p[i] = 'a'
		if pos := b.read + i; pos < len(prefix) {
			p[i] = prefix[pos]
		}
	}
	b.read += n
	return n, nil
}

// TestLogBodyCap checks debug logging reads ahead only the part of a request
// body it logs, so a body over the handlers' cap is never read past it and is
// rejected as with logging off.
func TestLogBodyCap(t *testing.T) {
	c := qt.New(t)
	logPath := logToFile(t)
	a := newTestAPI(t)
	oversized := func() (*http.Request, *capBody) {
		body := &capBody{}
		req := httptest.NewRequest(http.MethodPost, "/elections/5eed01/votes", body)
		req.ContentLength = 16 * maxRequestBody
		return req, body
	}

	DisabledLogging = true
	req, body := oversized()
	assertError(t, serve(a, req), ErrMalformedBody)
	c.Assert(body.read, qt.Equals, maxRequestBody)

	DisabledLogging = false
	req, body = oversized()
	assertError(t, serve(a, req), ErrMalformedBody)
	c.Assert(body.read, qt.Equals, maxRequestBody)
	logged, _ := readLog(t, logPath, 0)
	c.Assert(logged, qt.Contains, "{proof:aaa")

	req, body = oversized()
	readAhead := -1
	logging := loggingMiddleware(maxRequestBodyLog, func(*http.Request) bool { return false })
	logging(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		readAhead = body.read
	})).ServeHTTP(httptest.NewRecorder(), req)
	c.Assert(readAhead, qt.Equals, maxRequestBodyLog+1)
}
