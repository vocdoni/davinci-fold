package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/vocdoni/davinci-node/log"
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

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
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

// loggingMiddleware provides request/response logging for debugging.
func loggingMiddleware(maxBodyLog int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if DisabledLogging || log.Level() != log.LogLevelDebug {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			var bodyStr string
			if r.Body != nil && r.ContentLength > 0 {
				bodyBytes, err := io.ReadAll(r.Body)
				if err != nil {
					log.Error(err)
					http.Error(w, "unable to read request body", http.StatusInternalServerError)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				if jsonRegex.Match(bodyBytes) {
					bodyStr = string(bodyBytes)
					if len(bodyStr) > maxBodyLog {
						bodyStr = bodyStr[:maxBodyLog] + "..."
					}
					bodyStr = strings.ReplaceAll(bodyStr, "\"", "")
				}
			}
			wrapped := &responseWriter{ResponseWriter: w}
			log.Debugw("api request", "method", r.Method, "url", r.URL.String(), "body", bodyStr)
			next.ServeHTTP(wrapped, r)
			log.Debugw("api response",
				"method", r.Method, "url", r.URL.String(),
				"status", wrapped.statusCode, "took", time.Since(start).String())
		})
	}
}

// jwtAuth returns middleware enforcing a valid HMAC-signed JWT whose "role"
// claim is one of allowedRoles. The subject and role are stored in the request
// context for downstream handlers and the audit log.
func (a *API) jwtAuth(allowedRoles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedRoles))
	for _, role := range allowedRoles {
		allowed[role] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if raw == "" {
				ErrUnauthorized.With("missing bearer token").Write(w)
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
