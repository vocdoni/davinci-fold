package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v4"
	"github.com/vocdoni/davinci-fold/log"
)

// jsonRegex matches common JSON starting patterns.
var jsonRegex = regexp.MustCompile(`^\s*[\[{]`)

// ctxKey is the type for request-context keys set by middleware.
type ctxKey string

const (
	// ctxSubject holds the authenticated subject (JWT "sub" claim).
	ctxSubject ctxKey = "subject"
	// ctxRole holds the authenticated role (JWT "role" claim).
	ctxRole ctxKey = "role"
)

// Roles carried in JWT claims.
const (
	RoleAdmin     = "admin"
	RoleKeywarden = "keywarden"
)

// responseWriter wraps http.ResponseWriter to capture the status code. The
// logging middleware hands it to the handlers of the routes it logs; redacted
// marks a route whose response body must not be logged.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	redacted   bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if rw.statusCode == 0 {
		rw.statusCode = code
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.statusCode == 0 {
		rw.statusCode = http.StatusOK
	}
	return rw.ResponseWriter.Write(b)
}

// responseLogged reports whether what is written to w may be logged: the
// logging middleware is on (not DisabledLogging, debug level) and the route is
// not redacted.
func responseLogged(w http.ResponseWriter) bool {
	rw, ok := w.(*responseWriter)
	return ok && !rw.redacted
}

// loggingMiddleware provides request/response logging for debugging. Headers
// are never logged, and neither are the bodies of the requests redacted
// reports (and of their responses, see httpWriteJSON). Only the part of a
// body it logs is read ahead: the handler reads those bytes and then the rest
// of the body, under its own limit.
func loggingMiddleware(maxBodyLog int, redacted func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if DisabledLogging || log.Level() != log.LogLevelDebug {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			redact := redacted(r)
			var bodyStr string
			if redact {
				bodyStr = "(redacted)"
			} else if r.Body != nil && r.ContentLength > 0 {
				bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, int64(maxBodyLog)+1))
				if err != nil {
					log.Error(err)
					http.Error(w, "unable to read request body", http.StatusInternalServerError)
					return
				}
				r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(bodyBytes), r.Body))
				if jsonRegex.Match(bodyBytes) {
					bodyStr = string(bodyBytes)
					if len(bodyStr) > maxBodyLog {
						bodyStr = bodyStr[:maxBodyLog] + "..."
					}
					bodyStr = strings.ReplaceAll(bodyStr, "\"", "")
				}
			}
			wrapped := &responseWriter{ResponseWriter: w, redacted: redact}
			log.Debugw("api request", "method", r.Method, "url", r.URL.String(), "body", bodyStr)
			next.ServeHTTP(wrapped, r)
			log.Debugw("api response",
				"method", r.Method, "url", r.URL.String(),
				"status", wrapped.statusCode, "took", time.Since(start).String())
		})
	}
}

// redactedRoute reports whether the bodies of r must not be logged: it is
// routed to one of LogRedactedRoutes, or to no route at all, so a secret sent
// to a mistyped path is not logged either. It matches the path the router
// matches.
func (a *API) redactedRoute(r *http.Request) bool {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	pattern := a.router.Find(chi.NewRouteContext(), r.Method, path)
	return pattern == "" || slices.Contains(LogRedactedRoutes, r.Method+" "+pattern)
}

// jwtAuth returns middleware enforcing a valid HMAC-signed JWT with an expiry
// ("exp") whose "role" claim is one of allowedRoles: a missing, malformed,
// expired or never-expiring token is ErrInvalidToken (401), a valid token of
// another role ErrUnauthorized (403). The subject and role are stored in the
// request context for downstream handlers and the audit log.
func (a *API) jwtAuth(allowedRoles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedRoles))
	for _, role := range allowedRoles {
		allowed[role] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if raw == "" {
				ErrInvalidToken.With("missing bearer token").Write(w)
				return
			}
			claims := jwt.MapClaims{}
			token, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, jwt.ErrSignatureInvalid
				}
				return a.jwtSecret, nil
			})
			if err != nil || !token.Valid {
				ErrInvalidToken.Write(w)
				return
			}
			if !claims.VerifyExpiresAt(time.Now().Unix(), true) {
				ErrInvalidToken.With("the token must carry an exp claim").Write(w)
				return
			}
			role, _ := claims["role"].(string)
			if !allowed[role] {
				ErrUnauthorized.Withf("role %q not permitted", role).Write(w)
				return
			}
			sub, _ := claims["sub"].(string)
			ctx := context.WithValue(r.Context(), ctxSubject, sub)
			ctx = context.WithValue(ctx, ctxRole, role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// subjectFromContext returns the authenticated subject, if any.
func subjectFromContext(ctx context.Context) string {
	s, _ := ctx.Value(ctxSubject).(string)
	return s
}
