// Package logging is a thin wrapper over slog with structured-by-default
// behavior, log-file rotation hook, and redaction helpers.
package logging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Options struct {
	Level  string
	Format string
	File   string
}

type Logger = slog.Logger

type Closer interface{ Close() error }

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func New(opts Options) (*slog.Logger, Closer, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, nil, err
	}

	var w io.Writer = os.Stderr
	var closer Closer = nopCloser{}
	if opts.File != "" {
		if err := os.MkdirAll(filepath.Dir(opts.File), 0o700); err != nil {
			return nil, nil, err
		}
		f, err := os.OpenFile(opts.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, err
		}
		w = io.MultiWriter(os.Stderr, f)
		closer = f
	}

	handlerOpts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if isSensitiveKey(a.Key) {
				a.Value = slog.StringValue("***redacted***")
				return a
			}
			// Defense-in-depth: scrub secret-looking content from string
			// values even under benign/unexpected keys, since the key list
			// cannot anticipate every key a credential might be reflected
			// under.
			if a.Value.Kind() == slog.KindString {
				if scrubbed, changed := scrubSensitiveValue(a.Value.String()); changed {
					a.Value = slog.StringValue(scrubbed)
				}
			}
			return a
		},
	}

	var handler slog.Handler
	switch strings.ToLower(opts.Format) {
	case "text":
		handler = slog.NewTextHandler(w, handlerOpts)
	default:
		handler = slog.NewJSONHandler(w, handlerOpts)
	}
	return slog.New(handler), closer, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, errors.New("invalid log level: " + s)
	}
}

// redactedMarker replaces the secret portion of a scrubbed value.
const redactedMarker = "[REDACTED]"

// valueScrubbers matches secret-looking content inside string values. Each
// pattern captures a leading token (header name, scheme, or cookie name) in
// group 1 and the secret payload in the remainder, which is replaced with
// redactedMarker. These run regardless of the attribute key, as
// defense-in-depth against credentials reflected under unexpected keys.
var valueScrubbers = []*regexp.Regexp{
	// HTTP auth/cookie headers serialized as "Header: value" or "Header=value".
	regexp.MustCompile(`(?i)((?:cookie|set-cookie|authorization|proxy-authorization)\s*[:=]\s*).+`),
	// Authorization scheme prefixes anywhere in the value.
	regexp.MustCompile(`(?i)((?:bearer|basic)\s+)[A-Za-z0-9\-._~+/]+=*`),
	// Known Prism / IDP / servlet session cookies expressed as name=value. The
	// NTNX_*_SESSION family is matched generically (NTNX_IGW_SESSION,
	// NTNX_MERCURY_SESSION, and the live NTNX_MERCURY_IAM_SESSION — which the
	// old explicit list missed — plus any future Nutanix session cookie).
	regexp.MustCompile(`(?i)((?:NTNX_[A-Z0-9_]*SESSION|JSESSIONID|PRISM_SESSION|igw_session|x-ntnx-prism-token)\s*=\s*)[^;,\s]+`),
}

// scrubSensitiveValue redacts the secret portion of any secret-looking
// substrings within s. It returns the scrubbed string and whether any
// substitution was made.
func scrubSensitiveValue(s string) (string, bool) {
	out := s
	for _, re := range valueScrubbers {
		out = re.ReplaceAllString(out, "${1}"+redactedMarker)
	}
	return out, out != s
}

// Scrub redacts secret-looking content (auth/cookie headers, bearer/basic
// schemes, Prism/IDP session cookies) from a free-form string, applying the
// same value scrubbers slog uses. Exported so other sinks that write
// operator-supplied or upstream-reflected strings (e.g. the audit log's Reason)
// get the same defense-in-depth without duplicating the patterns.
func Scrub(s string) string {
	out, _ := scrubSensitiveValue(s)
	return out
}

func isSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	switch k {
	case "password", "secret", "token", "access_token", "refresh_token",
		"id_token", "authorization", "cookie", "api_key", "client_secret",
		"ipc_secret", "session_token", "x-ntnx-prism-token":
		return true
	}
	return strings.Contains(k, "secret") || strings.Contains(k, "password") || strings.Contains(k, "token")
}
