package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestAPIError_ErrorOmitsBody is the M-4 regression guard: an upstream WAF can
// reflect the request's Cookie/Authorization headers into a 4xx/5xx response
// body, and APIError.Error() flows into operator-facing toasts and the on-disk
// audit log. Error() must surface Method/Path/Status but NEVER the body. The
// body remains on the struct for programmatic branching (see images_gc.go).
func TestAPIError_ErrorOmitsBody(t *testing.T) {
	const secret = "NTNX_MERCURY_IAM_SESSION=leakedcookie9000"
	e := &APIError{
		Method: http.MethodGet,
		Path:   "/api/nutanix/v3/vms/x",
		Status: 401,
		Body:   "set-cookie: " + secret + "; HttpOnly",
	}
	msg := e.Error()
	if strings.Contains(msg, secret) || strings.Contains(msg, "set-cookie") {
		t.Fatalf("APIError.Error() leaked the response body: %q", msg)
	}
	// The safe, useful fields must still be present for diagnostics.
	for _, want := range []string{http.MethodGet, "/api/nutanix/v3/vms/x", "401"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("APIError.Error() should contain %q, got %q", want, msg)
		}
	}
	// The body must still be readable on the struct for callers that branch on it.
	if e.Body == "" {
		t.Fatal("APIError.Body should remain populated for programmatic inspection")
	}
}
