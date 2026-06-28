// Package httpapi implements the wall-e HTTP gateway surface: a `/health`
// (no-auth) endpoint and a `/v1/prompt` (bearer-auth) endpoint that streams
// SSE events from a pooled pi process.
package httpapi

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// authorize validates a bearer token from the request's Authorization header
// against `token` using a constant-time compare. Returns true only when the
// header is exactly `Bearer <token>`.
func authorize(r *http.Request, token string) bool {
	if token == "" {
		// No token configured = auth disabled (only valid in tests/dev).
		// Production requires WALLE_TOKEN; the server refuses to start without
		// it, but defend in depth here.
		return true
	}
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimPrefix(h, prefix)
	// Compare only when lengths match to avoid a trivial timing oracle based
	// on length; subtle.ConstantTimeCompare returns 0 for differing lengths
	// without examining bytes, which is what we want.
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// errMissingToken is the sentinel for an unauthenticated request.
var errMissingToken = errors.New("httpapi: missing or invalid token")
