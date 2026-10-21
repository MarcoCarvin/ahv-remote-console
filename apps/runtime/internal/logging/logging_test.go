package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// newTestLogger builds a JSON logger that writes into buf, exercising the
// same ReplaceAttr redaction path as the production logger.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	handlerOpts := &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if isSensitiveKey(a.Key) {
				a.Value = slog.StringValue("***redacted***")
				return a
			}
			if a.Value.Kind() == slog.KindString {
				if scrubbed, changed := scrubSensitiveValue(a.Value.String()); changed {
					a.Value = slog.StringValue(scrubbed)
				}
			}
			return a
		},
	}
	return slog.New(slog.NewJSONHandler(buf, handlerOpts))
}

// fieldValue extracts the string value of key from the (single-line) JSON log
// output captured in buf.
func fieldValue(t *testing.T, buf *bytes.Buffer, key string) string {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log output is not valid JSON: %v\noutput: %s", err, buf.String())
	}
	v, ok := rec[key]
	if !ok {
		t.Fatalf("log record missing key %q; output: %s", key, buf.String())
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("log key %q is not a string: %T", key, v)
	}
	return s
}

// TestReplaceAttr_KeyRedaction confirms the existing key-based behavior is
// preserved.
func TestReplaceAttr_KeyRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)
	log.Info("login", slog.String("password", "hunter2"))

	got := fieldValue(t, &buf, "password")
	if got != "***redacted***" {
		t.Fatalf("expected key-based redaction, got %q", got)
	}
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("secret leaked in output: %s", buf.String())
	}
}

// TestReplaceAttr_ValueScrubbing is the core defense-in-depth test: a
// secret-looking value reflected under a benign (non-listed) key must still be
// scrubbed.
func TestReplaceAttr_ValueScrubbing(t *testing.T) {
	cases := []struct {
		name       string
		benignKey  string
		value      string
		mustNotSee string // raw secret that must not appear in output
	}{
		{
			name:       "cookie header under benign key",
			benignKey:  "detail",
			value:      "Cookie: NTNX_IGW_SESSION=abc123deadbeef",
			mustNotSee: "abc123deadbeef",
		},
		{
			name:       "authorization bearer under benign key",
			benignKey:  "request",
			value:      "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
			mustNotSee: "eyJhbGciOiJIUzI1NiJ9.payload.sig",
		},
		{
			name:       "basic scheme under benign key",
			benignKey:  "header",
			value:      "Basic dXNlcjpwYXNzd29yZA==",
			mustNotSee: "dXNlcjpwYXNzd29yZA==",
		},
		{
			name:       "prism session cookie name=value under benign key",
			benignKey:  "msg",
			value:      "replaying NTNX_IGW_SESSION=s3cr3tvalue; path=/",
			mustNotSee: "s3cr3tvalue",
		},
		{
			name:       "jsessionid cookie under benign key",
			benignKey:  "context",
			value:      "JSESSIONID=0123456789ABCDEF",
			mustNotSee: "0123456789ABCDEF",
		},
		{
			// Regression for the M-4 finding: the live Prism cookie
			// NTNX_MERCURY_IAM_SESSION was NOT matched by the old explicit
			// alternation (which only listed NTNX_MERCURY_SESSION). The
			// generalized NTNX_*_SESSION pattern must catch it.
			name:       "mercury IAM session cookie (M-4 regression)",
			benignKey:  "reason",
			value:      "GET /api: HTTP 401: set-cookie: NTNX_MERCURY_IAM_SESSION=leakedtoken9000; HttpOnly",
			mustNotSee: "leakedtoken9000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := newTestLogger(&buf)
			log.Info("event", slog.String(tc.benignKey, tc.value))

			out := buf.String()
			if strings.Contains(out, tc.mustNotSee) {
				t.Fatalf("secret value leaked in cleartext: %s", out)
			}
			got := fieldValue(t, &buf, tc.benignKey)
			if !strings.Contains(got, redactedMarker) {
				t.Fatalf("expected %q to contain %q, got %q", tc.benignKey, redactedMarker, got)
			}
		})
	}
}

// TestReplaceAttr_BenignValueUntouched ensures ordinary string values are not
// mangled by the value scrubber.
func TestReplaceAttr_BenignValueUntouched(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)
	const benign = "connected to host prism.example.com on port 9440"
	log.Info("event", slog.String("detail", benign))

	got := fieldValue(t, &buf, "detail")
	if got != benign {
		t.Fatalf("benign value was altered: got %q want %q", got, benign)
	}
}

func TestScrubSensitiveValue_ReportsChange(t *testing.T) {
	if _, changed := scrubSensitiveValue("nothing sensitive here"); changed {
		t.Fatalf("benign value should not report a change")
	}
	if _, changed := scrubSensitiveValue("Authorization: Bearer xyz"); !changed {
		t.Fatalf("secret value should report a change")
	}
}

// TestScrub_ExportedRedactsSessionCookies covers the exported Scrub used by the
// audit log's Reason scrubbing. Each NTNX_*_SESSION variant must be redacted.
func TestScrub_ExportedRedactsSessionCookies(t *testing.T) {
	for _, name := range []string{"NTNX_IGW_SESSION", "NTNX_MERCURY_SESSION", "NTNX_MERCURY_IAM_SESSION"} {
		in := name + "=topsecretvalue123"
		out := Scrub(in)
		if strings.Contains(out, "topsecretvalue123") {
			t.Fatalf("%s value leaked through Scrub: %q", name, out)
		}
		if !strings.Contains(out, redactedMarker) {
			t.Fatalf("%s not redacted: %q", name, out)
		}
	}
	if got := Scrub("connected to prism on 9440"); got != "connected to prism on 9440" {
		t.Fatalf("benign string altered: %q", got)
	}
}
