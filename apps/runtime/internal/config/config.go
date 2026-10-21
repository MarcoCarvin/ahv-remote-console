// Package config loads, validates, and persists runtime configuration. The
// config file is JSON at <data-dir>/config.json with 0600 permissions. All
// secrets are stored in the OS keystore, never in this file.
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Config struct {
	Version      int               `json:"version"`
	UI           UISettings        `json:"ui"`
	Network      NetworkSettings   `json:"network"`
	Security     SecuritySettings  `json:"security"`
	Telemetry    TelemetrySettings `json:"telemetry"`
	Endpoints    []Endpoint        `json:"endpoints"`
	FeatureFlags map[string]bool   `json:"feature_flags"`
}

type UISettings struct {
	Theme           string `json:"theme"` // dark | light | system
	ReduceMotion    bool   `json:"reduce_motion"`
	HotkeyOpenPanel string `json:"hotkey_open_panel"`
	AutoLockMinutes int    `json:"auto_lock_minutes"` // 0 = disabled
}

type NetworkSettings struct {
	ProxyURL            string   `json:"proxy_url"`
	NoProxy             []string `json:"no_proxy"`
	HeartbeatSeconds    int      `json:"heartbeat_seconds"`
	ReconnectMaxSeconds int      `json:"reconnect_max_seconds"`
}

type SecuritySettings struct {
	EnforceTLS13         bool     `json:"enforce_tls13"`
	CustomCAFiles        []string `json:"custom_ca_files"`
	CertificatePinning   bool     `json:"certificate_pinning"`
	AllowSelfSigned      bool     `json:"allow_self_signed"` // dev only, surfaced as warning
	AuditTrailEnabled    bool     `json:"audit_trail_enabled"`
	ClipboardHistoryHide bool     `json:"clipboard_history_hide"`
	RequireOSScreenLock  bool     `json:"require_os_screen_lock"`
}

type TelemetrySettings struct {
	Enabled      bool    `json:"enabled"`
	OTLPEndpoint string  `json:"otlp_endpoint"`
	SampleRate   float64 `json:"sample_rate"`
}

// AuthKind names an authentication mechanism. The platform ships exactly one:
//
//   - "cookie" — Embedded SSO. AHV Remote Console opens Prism's /console/ login
//     in a sandboxed BrowserWindow, the user authenticates via
//     whatever IdP Prism has configured (Okta, Authentik, ...),
//     and the captured session cookies are replayed on every
//     upstream API call.
type AuthKind string

const (
	AuthKindCookie AuthKind = "cookie"
)

type Endpoint struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	PluginID string `json:"plugin_id"`
	URL      string `json:"url"`
	// AuthMethods lists the authentication mechanisms enabled for this
	// endpoint. An empty list is treated as ["cookie"].
	AuthMethods      []AuthKind        `json:"auth_methods"`
	PinnedCertB64    string            `json:"pinned_cert_b64,omitempty"`
	AllowInsecureTLS bool              `json:"allow_insecure_tls,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
}

// Allows returns true if `kind` is one of the endpoint's enabled methods.
func (e Endpoint) Allows(kind AuthKind) bool {
	if len(e.AuthMethods) == 0 {
		return kind == AuthKindCookie
	}
	for _, k := range e.AuthMethods {
		if k == kind {
			return true
		}
	}
	return false
}

// Validate runs the per-endpoint invariants (URL is HTTPS, pinned
// cert is plausible base64-DER, auth methods are known). Returns a
// ValidationErrors aggregate or nil. Does NOT enforce ID uniqueness
// — that's a cross-endpoint invariant only enforceable from the
// owning Config.
//
// Used by Manager.UpsertEndpoint (closes TD-034 — IPC path used to
// bypass Validate). Config.Validate uses validateInto with a path
// prefix to compose multiple endpoint errors into one report.
func (e Endpoint) Validate() error {
	errs := e.validateInto("endpoint")
	if len(errs) == 0 {
		return nil
	}
	return ValidationErrors(errs)
}

// validateInto appends the per-endpoint validation errors to the
// returned slice, prefixed by `pathBase` (e.g. "endpoints[2]" when
// called from Config.Validate, or "endpoint" when called from the
// single-endpoint Endpoint.Validate path).
func (e Endpoint) validateInto(pathBase string) []ValidationError {
	var errs []ValidationError

	if strings.TrimSpace(e.ID) == "" {
		errs = append(errs, ValidationError{Path: pathBase + ".id", Msg: "must be non-empty"})
	}

	if strings.TrimSpace(e.URL) == "" {
		errs = append(errs, ValidationError{Path: pathBase + ".url", Msg: "must be non-empty"})
	} else {
		u, perr := url.Parse(e.URL)
		switch {
		case perr != nil:
			errs = append(errs, ValidationError{Path: pathBase + ".url", Msg: fmt.Sprintf("invalid URL: %v", perr)})
		case u.Scheme != "https":
			errs = append(errs, ValidationError{Path: pathBase + ".url", Msg: fmt.Sprintf("must use https://, got scheme %q", u.Scheme)})
		case u.Host == "":
			errs = append(errs, ValidationError{Path: pathBase + ".url", Msg: "URL has no host"})
		}
	}

	if e.PinnedCertB64 != "" {
		raw, derr := base64.StdEncoding.DecodeString(e.PinnedCertB64)
		if derr != nil {
			errs = append(errs, ValidationError{Path: pathBase + ".pinned_cert_b64", Msg: fmt.Sprintf("not valid base64: %v", derr)})
		} else if len(raw) < 100 {
			errs = append(errs, ValidationError{Path: pathBase + ".pinned_cert_b64", Msg: fmt.Sprintf("decoded payload is %d bytes; a DER X.509 cert is typically >300 bytes — looks malformed", len(raw))})
		}
	}

	// allow_insecure_tls without a pinned_cert_b64 is accepted. It marks an
	// endpoint that awaits trust on first use: the embedded SSO window captures
	// the cluster's certificate, the operator confirms its SHA-256 fingerprint,
	// and the renderer then stores the pin and clears allow_insecure_tls. Until a
	// pin exists the runtime never skips verification: the AHV plugin's
	// tlsConfigFor refuses every handshake for this combination, so the endpoint
	// stays unusable rather than MITM-able.

	for j, m := range e.AuthMethods {
		switch m {
		case AuthKindCookie:
			// ok
		default:
			errs = append(errs, ValidationError{Path: fmt.Sprintf("%s.auth_methods[%d]", pathBase, j), Msg: fmt.Sprintf("unknown auth kind %q (want cookie)", m)})
		}
	}

	return errs
}

const currentVersion = 1

func defaults() Config {
	return Config{
		Version: currentVersion,
		UI: UISettings{
			Theme:           "dark",
			HotkeyOpenPanel: "CmdOrCtrl+Shift+H",
			AutoLockMinutes: 0,
		},
		Network: NetworkSettings{
			HeartbeatSeconds:    15,
			ReconnectMaxSeconds: 30,
		},
		Security: SecuritySettings{
			EnforceTLS13:       true,
			CertificatePinning: false,
			AuditTrailEnabled:  true,
		},
		Telemetry: TelemetrySettings{
			Enabled:    false,
			SampleRate: 0.1,
		},
		FeatureFlags: map[string]bool{
			"command_palette":    true,
			"multi_monitor":      true,
			"paste_as_typing":    true,
			"diagnostics_bundle": true,
			"presence":           false,
			"remote_cursor":      false,
			"chat":               false,
			// QEMU Extended Key Event (-258): send raw scancodes so the GUEST
			// applies its own keyboard layout / input language (RDP-style).
			// DEFAULT OFF — AHV's QEMU build mis-handles CLI_MSG_QEMU (msg 255)
			// and corrupts the RFB stream ("Unsupported RFB encoding: -258 …
			// stream misaligned"), so the console fails to connect. Opt in ONLY
			// after validating the extension against the target cluster's QEMU.
			"qemu_ext_key_event": false,
			// Reliable large clipboard paste: on a paste too big for the direct
			// path, probe the guest's receive capacity (HELLO handshake) so the
			// host sends a few big BCP frames instead of hundreds of 2 KB frames
			// whose per-frame ACKs contend on QEMU's single clipboard slot and
			// time out. Affects ONLY the large-paste path (small paste is
			// unchanged). Enabled for validation; revisit before GA.
			"clipboard_large_paste": true,
		},
	}
}

type Manager struct {
	mu   sync.RWMutex
	path string
	cfg  Config
	log  *slog.Logger
}

func Load(path string, log *slog.Logger) (*Manager, error) {
	m := &Manager{path: path, cfg: defaults(), log: log}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := m.Save(); err != nil {
			return nil, err
		}
		log.Info("config initialized with defaults", "path", path)
		return m, nil
	case err != nil:
		return nil, err
	}
	var disk Config
	if err := json.Unmarshal(data, &disk); err != nil {
		return nil, err
	}
	if disk.Version == 0 {
		disk.Version = currentVersion
	}
	if disk.FeatureFlags == nil {
		disk.FeatureFlags = defaults().FeatureFlags
	}
	migrated := migrateEndpoints(&disk)
	m.cfg = disk
	if migrated {
		log.Info("config migrated to current auth-methods schema", "path", path)
		if err := m.Save(); err != nil {
			log.Warn("config migration save failed", "err", err)
		}
	}

	// TD-033: surface validation issues at startup. Soft-fail for
	// now (log at WARN, continue) so that operators with weird-but-
	// functional configs aren't locked out by a new invariant. Future
	// hard-fail can be gated on a `--strict-config` flag once we have
	// field experience with the validator. The validator is aggregate
	// — every issue is reported in one pass.
	if verr := m.cfg.Validate(); verr != nil {
		log.Warn("config validation found issues — continuing anyway", "errors", verr.Error())
	}

	return m, nil
}

// migrateEndpoints rewrites any pre-v1.1 endpoints that still list the
// retired "oidc" auth method to use "cookie" instead. Returns true if any
// endpoint was changed so the caller can persist.
func migrateEndpoints(c *Config) bool {
	changed := false
	for i, e := range c.Endpoints {
		methods := make([]AuthKind, 0, len(e.AuthMethods))
		seen := map[AuthKind]bool{}
		mutated := false
		for _, m := range e.AuthMethods {
			// Retired methods fold into cookie (embedded SSO): "oidc" was a
			// rename; "basic" (app-collected credentials) is gone now that the
			// platform leverages the existing PSC session captured via SSO.
			if m == AuthKind("oidc") || m == AuthKind("basic") {
				m = AuthKindCookie
				mutated = true
			}
			if !seen[m] {
				methods = append(methods, m)
				seen[m] = true
			}
		}
		if mutated {
			c.Endpoints[i].AuthMethods = methods
			changed = true
		}
	}
	return changed
}

func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Manager) Update(fn func(*Config)) error {
	m.mu.Lock()
	fn(&m.cfg)
	m.mu.Unlock()
	return m.Save()
}

func (m *Manager) Save() error {
	m.mu.RLock()
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func (m *Manager) Endpoint(id string) (Endpoint, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.cfg.Endpoints {
		if e.ID == id {
			return e, true
		}
	}
	return Endpoint{}, false
}

// UpsertEndpoint inserts or replaces an endpoint by ID. If ID is empty, a
// stable slug is derived from the label so operators can be terse in the UI.
func (m *Manager) UpsertEndpoint(e Endpoint) (Endpoint, error) {
	// Quick presence checks first — these produce more actionable
	// error messages than the field-path-style Validate output
	// when the caller has obviously forgotten a required field.
	if e.URL == "" {
		return Endpoint{}, errors.New("endpoint URL required")
	}
	if e.PluginID == "" {
		return Endpoint{}, errors.New("plugin_id required")
	}

	// Default fill before validation: empty AuthMethods means
	// "both basic and cookie allowed", and an empty ID is derived
	// from the label/URL. These need to happen BEFORE Validate
	// because otherwise we'd reject our own defaults.
	if len(e.AuthMethods) == 0 {
		e.AuthMethods = []AuthKind{AuthKindCookie}
	}
	if e.ID == "" {
		e.ID = slug(e.Label)
		if e.ID == "" {
			e.ID = slug(e.URL)
		}
	}

	// TD-034: enforce the same per-endpoint invariants that
	// Config.Validate enforces. Previously this path only checked
	// URL + plugin_id and silently accepted http://, malformed
	// pinned certs, unknown auth methods. The IPC `endpoint.upsert`
	// channel routes here.
	if verr := e.Validate(); verr != nil {
		return Endpoint{}, fmt.Errorf("invalid endpoint: %w", verr)
	}

	m.mu.Lock()
	replaced := false
	for i, existing := range m.cfg.Endpoints {
		if existing.ID == e.ID {
			m.cfg.Endpoints[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		m.cfg.Endpoints = append(m.cfg.Endpoints, e)
	}
	m.mu.Unlock()
	return e, m.Save()
}

func (m *Manager) DeleteEndpoint(id string) error {
	m.mu.Lock()
	out := m.cfg.Endpoints[:0]
	removed := false
	for _, e := range m.cfg.Endpoints {
		if e.ID == id {
			removed = true
			continue
		}
		out = append(out, e)
	}
	m.cfg.Endpoints = out
	m.mu.Unlock()
	if !removed {
		return errors.New("endpoint not found")
	}
	return m.Save()
}

func slug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// ----------------------------------------------------------------------------
// Validation
// ----------------------------------------------------------------------------

// ValidationError reports a single problem in the configuration, scoped to
// a JSON-ish field path (e.g. "endpoints[2].url") so an operator can fix it
// without grepping.
type ValidationError struct {
	Path string
	Msg  string
}

func (e ValidationError) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

// ValidationErrors aggregates multiple ValidationError so Validate can report
// every problem in one pass instead of stopping at the first.
type ValidationErrors []ValidationError

func (es ValidationErrors) Error() string {
	switch len(es) {
	case 0:
		return ""
	case 1:
		return es[0].Error()
	}
	parts := make([]string, 0, len(es))
	for _, e := range es {
		parts = append(parts, e.Error())
	}
	return fmt.Sprintf("config invalid: %d problems: %s", len(es), strings.Join(parts, "; "))
}

// Has returns true if any error's path matches the given prefix. Useful for
// tests that want to assert "an endpoints[1].url error was raised" without
// pinning to the exact message.
func (es ValidationErrors) Has(pathPrefix string) bool {
	for _, e := range es {
		if strings.HasPrefix(e.Path, pathPrefix) {
			return true
		}
	}
	return false
}

// knownFeatureFlags enumerates flags this binary recognises. Unknown flags in
// the config file are silently kept (forward-compat with newer runtimes) but
// known flags are required to be present-as-bool by the JSON schema; since
// the Go struct uses map[string]bool, non-bool values are caught at
// json.Unmarshal time by the json package itself. The set below is used only
// for documentation / introspection.
var _ = knownFeatureFlags // used for documentation/introspection
var knownFeatureFlags = map[string]struct{}{
	"command_palette":       {},
	"multi_monitor":         {},
	"paste_as_typing":       {},
	"diagnostics_bundle":    {},
	"presence":              {},
	"remote_cursor":         {},
	"chat":                  {},
	"qemu_ext_key_event":    {},
	"clipboard_large_paste": {},
}

func (c Config) FeatureEnabled(name string) bool {
	v, ok := c.FeatureFlags[name]
	return ok && v
}

// Validate walks the Config and returns a ValidationErrors aggregate. A nil
// return means the config is acceptable; an empty config (zero endpoints, all
// defaults) is acceptable. Validate never mutates the Config.
func (c Config) Validate() error {
	var errs ValidationErrors

	// Version: 0 means "unset, treat as current". Anything > currentVersion
	// is a sign of a downgrade we shouldn't attempt to interpret.
	if c.Version < 0 {
		errs = append(errs, ValidationError{Path: "version", Msg: "must be >= 0"})
	}
	if c.Version > currentVersion {
		errs = append(errs, ValidationError{Path: "version", Msg: fmt.Sprintf("unknown config version %d (this runtime understands up to %d)", c.Version, currentVersion)})
	}

	// UI
	if c.UI.AutoLockMinutes < 0 || c.UI.AutoLockMinutes > 1440 {
		errs = append(errs, ValidationError{Path: "ui.auto_lock_minutes", Msg: fmt.Sprintf("must be in [0, 1440], got %d", c.UI.AutoLockMinutes)})
	}
	switch c.UI.Theme {
	case "", "dark", "light", "system":
		// ok
	default:
		errs = append(errs, ValidationError{Path: "ui.theme", Msg: fmt.Sprintf("must be one of dark|light|system, got %q", c.UI.Theme)})
	}

	// Network
	if c.Network.HeartbeatSeconds != 0 {
		if c.Network.HeartbeatSeconds < 1 || c.Network.HeartbeatSeconds > 3600 {
			errs = append(errs, ValidationError{Path: "network.heartbeat_seconds", Msg: fmt.Sprintf("must be in [1, 3600], got %d", c.Network.HeartbeatSeconds)})
		}
	}
	if c.Network.ReconnectMaxSeconds != 0 {
		if c.Network.ReconnectMaxSeconds < 1 || c.Network.ReconnectMaxSeconds > 3600 {
			errs = append(errs, ValidationError{Path: "network.reconnect_max_seconds", Msg: fmt.Sprintf("must be in [1, 3600], got %d", c.Network.ReconnectMaxSeconds)})
		}
	}
	if c.Network.ProxyURL != "" {
		if _, perr := url.Parse(c.Network.ProxyURL); perr != nil {
			errs = append(errs, ValidationError{Path: "network.proxy_url", Msg: fmt.Sprintf("invalid URL: %v", perr)})
		}
	}

	// Telemetry
	if c.Telemetry.SampleRate < 0 || c.Telemetry.SampleRate > 1 {
		errs = append(errs, ValidationError{Path: "telemetry.sample_rate", Msg: fmt.Sprintf("must be in [0.0, 1.0], got %v", c.Telemetry.SampleRate)})
	}
	if c.Telemetry.Enabled && strings.TrimSpace(c.Telemetry.OTLPEndpoint) == "" {
		errs = append(errs, ValidationError{Path: "telemetry.otlp_endpoint", Msg: "must be set when telemetry.enabled=true"})
	}
	if c.Telemetry.OTLPEndpoint != "" {
		if u, perr := url.Parse(c.Telemetry.OTLPEndpoint); perr != nil || u.Scheme == "" {
			errs = append(errs, ValidationError{Path: "telemetry.otlp_endpoint", Msg: fmt.Sprintf("invalid URL %q", c.Telemetry.OTLPEndpoint)})
		}
	}

	// Security
	for i, p := range c.Security.CustomCAFiles {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, ValidationError{Path: fmt.Sprintf("security.custom_ca_files[%d]", i), Msg: "must be a non-empty path"})
		}
	}

	// Endpoints: per-endpoint invariants extracted into Endpoint.Validate
	// so Manager.UpsertEndpoint can share the same checks. See TD-034.
	seen := map[string]int{}
	for i, e := range c.Endpoints {
		base := fmt.Sprintf("endpoints[%d]", i)

		// Per-endpoint invariants (id non-empty, url, pinned cert, auth).
		errs = append(errs, e.validateInto(base)...)

		// Cross-endpoint invariant: ID uniqueness (only enforceable from
		// the full list).
		if strings.TrimSpace(e.ID) != "" {
			if prior, ok := seen[e.ID]; ok {
				errs = append(errs, ValidationError{Path: base + ".id", Msg: fmt.Sprintf("duplicate of endpoints[%d].id (%q)", prior, e.ID)})
			} else {
				seen[e.ID] = i
			}
		}
	}

	// FeatureFlags: unknown keys are silently allowed; map[string]bool means
	// non-bool values are rejected at unmarshal time, so nothing to check
	// here except that the map is sane (no empty keys).
	for k := range c.FeatureFlags {
		if strings.TrimSpace(k) == "" {
			errs = append(errs, ValidationError{Path: "feature_flags", Msg: "feature flag has empty key"})
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}
