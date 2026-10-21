// config_test.go covers JSON marshalling, the Load file path (defaults +
// migration), and Validate. The codebase audit flagged config.go as a
// GA-blocker for "invalid configs silently accepted", so the bulk of this
// file is table-driven Validate cases.
//
// Tests are pure Go — no syscalls beyond stdlib os.* on t.TempDir(), so they
// run identically on Linux/Windows/macOS.

package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// discardLogger returns a slog.Logger that throws everything away so tests
// don't pollute test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// goodDER is a 256-byte filler used as the decoded payload for a "looks like
// a cert" base64 blob in tests. The validator only checks length, not ASN.1
// structure, so this is fine.
func goodCertB64() string {
	raw := make([]byte, 256)
	for i := range raw {
		raw[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// validEndpoint returns a fully-populated endpoint that should pass Validate.
func validEndpoint(id, label string) Endpoint {
	return Endpoint{
		ID:          id,
		Label:       label,
		PluginID:    "ahv",
		URL:         "https://prism.example.com:9440",
		AuthMethods: []AuthKind{AuthKindCookie},
	}
}

// validConfig returns a Config with every section populated and Validate-clean.
func validConfig() Config {
	c := defaults()
	c.Endpoints = []Endpoint{validEndpoint("prod", "Prod cluster")}
	return c
}

// --- Round-trip & loading --------------------------------------------------

func TestConfig_RoundTrip_MarshalUnmarshal(t *testing.T) {
	in := validConfig()
	in.Endpoints[0].PinnedCertB64 = goodCertB64()
	in.Endpoints[0].Tags = map[string]string{"env": "prod"}
	in.Security.CustomCAFiles = []string{"/etc/ssl/custom.pem"}
	in.Telemetry.Enabled = true
	in.Telemetry.OTLPEndpoint = "https://otel.example.com"

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Version != in.Version {
		t.Errorf("Version: got %d want %d", out.Version, in.Version)
	}
	if len(out.Endpoints) != 1 || out.Endpoints[0].ID != "prod" {
		t.Errorf("Endpoints did not round-trip: %+v", out.Endpoints)
	}
	if out.Endpoints[0].PinnedCertB64 != in.Endpoints[0].PinnedCertB64 {
		t.Errorf("PinnedCertB64 lost on round-trip")
	}
	if out.Endpoints[0].Tags["env"] != "prod" {
		t.Errorf("Tags lost on round-trip: %+v", out.Endpoints[0].Tags)
	}
	if out.UI.Theme != in.UI.Theme {
		t.Errorf("UI.Theme: got %q want %q", out.UI.Theme, in.UI.Theme)
	}
	if out.FeatureFlags["command_palette"] != true {
		t.Errorf("FeatureFlags lost on round-trip: %+v", out.FeatureFlags)
	}
}

func TestConfig_LoadValid_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	in := validConfig()
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m, err := Load(path, discardLogger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := m.Get()
	if len(got.Endpoints) != 1 || got.Endpoints[0].ID != "prod" {
		t.Errorf("loaded endpoints wrong: %+v", got.Endpoints)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("loaded config did not validate: %v", err)
	}
}

func TestConfig_LoadInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Load(path, discardLogger())
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid") && !strings.Contains(err.Error(), "unexpected") &&
		!strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "looking for") {
		t.Errorf("error not from json decoder: %v", err)
	}
}

func TestConfig_Load_CreatesDefaultsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	m, err := Load(path, discardLogger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected Load to create the file: %v", err)
	}
	got := m.Get()
	def := defaults()
	if got.Version != def.Version {
		t.Errorf("Version: got %d want %d", got.Version, def.Version)
	}
	if got.UI.Theme != def.UI.Theme {
		t.Errorf("UI.Theme: got %q want %q", got.UI.Theme, def.UI.Theme)
	}
	if got.Network.HeartbeatSeconds != def.Network.HeartbeatSeconds {
		t.Errorf("HeartbeatSeconds: got %d want %d", got.Network.HeartbeatSeconds, def.Network.HeartbeatSeconds)
	}
}

func TestConfig_Load_RejectsNonBoolFeatureFlag(t *testing.T) {
	// FeatureFlags is map[string]bool; a string value must be rejected by
	// the JSON decoder. Verifies the policy "known flags accept only bools"
	// is enforced structurally rather than at Validate time.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"version":1,"feature_flags":{"command_palette":"yes"}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Load(path, discardLogger())
	if err == nil {
		t.Fatalf("expected type error for non-bool feature flag")
	}
}

func TestConfig_DefaultValues(t *testing.T) {
	d := defaults()
	if d.Version != currentVersion {
		t.Errorf("Version: got %d want %d", d.Version, currentVersion)
	}
	if d.UI.Theme != "dark" {
		t.Errorf("UI.Theme: got %q want dark", d.UI.Theme)
	}
	if d.UI.AutoLockMinutes != 0 {
		t.Errorf("UI.AutoLockMinutes: got %d want 0", d.UI.AutoLockMinutes)
	}
	if d.UI.HotkeyOpenPanel != "CmdOrCtrl+Shift+H" {
		t.Errorf("UI.HotkeyOpenPanel: got %q", d.UI.HotkeyOpenPanel)
	}
	if d.Network.HeartbeatSeconds != 15 {
		t.Errorf("HeartbeatSeconds: got %d want 15", d.Network.HeartbeatSeconds)
	}
	if d.Network.ReconnectMaxSeconds != 30 {
		t.Errorf("ReconnectMaxSeconds: got %d want 30", d.Network.ReconnectMaxSeconds)
	}
	if !d.Security.EnforceTLS13 {
		t.Errorf("EnforceTLS13: want true")
	}
	if !d.Security.AuditTrailEnabled {
		t.Errorf("AuditTrailEnabled: want true")
	}
	if d.Telemetry.Enabled {
		t.Errorf("Telemetry.Enabled: want false")
	}
	if d.Telemetry.SampleRate != 0.1 {
		t.Errorf("SampleRate: got %v want 0.1", d.Telemetry.SampleRate)
	}
	for _, k := range []string{"command_palette", "multi_monitor", "paste_as_typing", "diagnostics_bundle"} {
		if !d.FeatureFlags[k] {
			t.Errorf("default FeatureFlags missing %q", k)
		}
	}
	if err := d.Validate(); err != nil {
		t.Errorf("defaults() failed Validate: %v", err)
	}
}

// --- Validate happy path & empty case --------------------------------------

func TestConfig_Validate_Empty(t *testing.T) {
	// A zero-value Config — no endpoints, no version, no flags — must
	// validate. The runtime treats "empty" as "use defaults" and we don't
	// want first-run users to see a validation error before they've added
	// anything.
	var c Config
	if err := c.Validate(); err != nil {
		t.Errorf("empty Config failed Validate: %v", err)
	}
}

func TestConfig_Validate_Defaults(t *testing.T) {
	if err := defaults().Validate(); err != nil {
		t.Errorf("defaults() failed Validate: %v", err)
	}
}

func TestConfig_Validate_HappyPath(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Errorf("validConfig() failed Validate: %v", err)
	}
}

// --- Validate: endpoints ---------------------------------------------------

func TestConfig_Validate_DuplicateEndpointIDs(t *testing.T) {
	c := defaults()
	c.Endpoints = []Endpoint{
		validEndpoint("dup", "First"),
		validEndpoint("dup", "Second"),
	}
	err := c.Validate()
	if err == nil {
		t.Fatalf("expected error for duplicate endpoint ID, got nil")
	}
	ve, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
	}
	if !ve.Has("endpoints[1].id") {
		t.Errorf("expected endpoints[1].id error, got %v", ve)
	}
}

func TestConfig_Validate_EmptyEndpointID(t *testing.T) {
	c := defaults()
	c.Endpoints = []Endpoint{validEndpoint("", "Anon")}
	err := c.Validate().(ValidationErrors)
	if !err.Has("endpoints[0].id") {
		t.Errorf("expected endpoints[0].id error, got %v", err)
	}
}

func TestConfig_Validate_EndpointURL_TableDriven(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid_https", "https://prism.example.com:9440", false},
		{"valid_https_with_path", "https://prism.example.com/api", false},
		{"http_rejected", "http://prism.example.com", true},
		{"ftp_rejected", "ftp://prism.example.com", true},
		{"ws_rejected", "ws://prism.example.com", true},
		{"empty_rejected", "", true},
		{"scheme_only_rejected", "https://", true},
		{"missing_scheme_rejected", "prism.example.com", true},
		{"garbage_rejected", "not-a-url", true},
		{"control_chars_rejected", "https://prism\x00.example.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := defaults()
			c.Endpoints = []Endpoint{validEndpoint("ep", "EP")}
			c.Endpoints[0].URL = tt.url
			err := c.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for URL %q, got nil", tt.url)
				}
				ve, ok := err.(ValidationErrors)
				if !ok {
					t.Fatalf("expected ValidationErrors, got %T", err)
				}
				if !ve.Has("endpoints[0].url") {
					t.Errorf("expected endpoints[0].url error, got %v", ve)
				}
			} else if err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

func TestConfig_Validate_MalformedPinnedCert(t *testing.T) {
	tests := []struct {
		name    string
		val     string
		wantErr bool
	}{
		{"empty_ok", "", false},
		{"valid_long_b64", goodCertB64(), false},
		{"not_base64", "!!! not valid base64 !!!", true},
		{"too_short", base64.StdEncoding.EncodeToString([]byte("short")), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := defaults()
			c.Endpoints = []Endpoint{validEndpoint("ep", "EP")}
			c.Endpoints[0].PinnedCertB64 = tt.val
			err := c.Validate()
			if tt.wantErr {
				ve, ok := err.(ValidationErrors)
				if !ok {
					t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
				}
				if !ve.Has("endpoints[0].pinned_cert_b64") {
					t.Errorf("expected endpoints[0].pinned_cert_b64 error, got %v", ve)
				}
			} else if err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

func TestConfig_Validate_InsecureWithoutPinAwaitsFirstUseTrust(t *testing.T) {
	// An endpoint marked allow_insecure_tls with no pin is saved so the SSO
	// window can capture the certificate for trust on first use. Connections
	// stay refused until a pin is stored (see plugins/ahv
	// TestFailClosed_onAllowInsecureWithoutPin).
	c := defaults()
	c.Endpoints = []Endpoint{validEndpoint("ep", "EP")}
	c.Endpoints[0].AllowInsecureTLS = true
	if err := c.Validate(); err != nil {
		t.Errorf("expected insecure-without-pin to validate, got %v", err)
	}
}

func TestConfig_Validate_AuthMethods(t *testing.T) {
	c := defaults()
	c.Endpoints = []Endpoint{validEndpoint("ep", "EP")}
	c.Endpoints[0].AuthMethods = []AuthKind{AuthKind("oidc")}
	err := c.Validate().(ValidationErrors)
	if !err.Has("endpoints[0].auth_methods[0]") {
		t.Errorf("expected auth_methods error, got %v", err)
	}

	// Empty AuthMethods is fine — semantically means "cookie" (SSO).
	c.Endpoints[0].AuthMethods = nil
	if err := c.Validate(); err != nil {
		t.Errorf("empty auth_methods should be allowed, got %v", err)
	}
}

// --- Validate: numeric range checks ----------------------------------------

func TestConfig_Validate_HeartbeatOutOfRange(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"zero_ok_means_default", 0, false}, // 0 = "use default"
		{"one_ok", 1, false},
		{"sane_value", 15, false},
		{"upper_bound", 3600, false},
		{"negative", -1, true},
		{"too_large", 100000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := defaults()
			c.Network.HeartbeatSeconds = tt.val
			err := c.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for HeartbeatSeconds=%d", tt.val)
				}
				ve := err.(ValidationErrors)
				if !ve.Has("network.heartbeat_seconds") {
					t.Errorf("expected network.heartbeat_seconds error, got %v", ve)
				}
			} else if err != nil {
				t.Errorf("expected no error for HeartbeatSeconds=%d, got %v", tt.val, err)
			}
		})
	}
}

func TestConfig_Validate_AutoLockOutOfRange(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"zero_disabled", 0, false},
		{"five", 5, false},
		{"max_24h", 1440, false},
		{"negative", -1, true},
		{"too_large", 1441, true},
		{"way_too_large", 100000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := defaults()
			c.UI.AutoLockMinutes = tt.val
			err := c.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for AutoLockMinutes=%d", tt.val)
				}
				ve := err.(ValidationErrors)
				if !ve.Has("ui.auto_lock_minutes") {
					t.Errorf("expected ui.auto_lock_minutes error, got %v", ve)
				}
			} else if err != nil {
				t.Errorf("expected no error for AutoLockMinutes=%d, got %v", tt.val, err)
			}
		})
	}
}

func TestConfig_Validate_TelemetrySampleRate(t *testing.T) {
	tests := []struct {
		name    string
		val     float64
		wantErr bool
	}{
		{"zero", 0, false},
		{"half", 0.5, false},
		{"one", 1, false},
		{"negative", -0.1, true},
		{"over_one", 1.1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := defaults()
			c.Telemetry.SampleRate = tt.val
			err := c.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for SampleRate=%v", tt.val)
				}
				ve := err.(ValidationErrors)
				if !ve.Has("telemetry.sample_rate") {
					t.Errorf("expected telemetry.sample_rate error, got %v", ve)
				}
			} else if err != nil {
				t.Errorf("expected no error for SampleRate=%v, got %v", tt.val, err)
			}
		})
	}
}

func TestConfig_Validate_TelemetryEnabledRequiresEndpoint(t *testing.T) {
	c := defaults()
	c.Telemetry.Enabled = true
	c.Telemetry.OTLPEndpoint = ""
	err := c.Validate().(ValidationErrors)
	if !err.Has("telemetry.otlp_endpoint") {
		t.Errorf("expected telemetry.otlp_endpoint error when Enabled and no endpoint, got %v", err)
	}

	c.Telemetry.OTLPEndpoint = "https://otel.example.com"
	if err := c.Validate(); err != nil {
		t.Errorf("expected no error when Enabled with valid endpoint, got %v", err)
	}
}

// --- Validate: ui.theme ----------------------------------------------------

func TestConfig_Validate_UITheme(t *testing.T) {
	for _, ok := range []string{"", "dark", "light", "system"} {
		c := defaults()
		c.UI.Theme = ok
		if err := c.Validate(); err != nil {
			t.Errorf("theme %q should be allowed, got %v", ok, err)
		}
	}
	c := defaults()
	c.UI.Theme = "neon"
	err := c.Validate().(ValidationErrors)
	if !err.Has("ui.theme") {
		t.Errorf("expected ui.theme error, got %v", err)
	}
}

// --- Validate: security.custom_ca_files ------------------------------------

func TestConfig_Validate_CustomCAFiles(t *testing.T) {
	c := defaults()
	c.Security.CustomCAFiles = []string{"/etc/ssl/ok.pem", "  ", "/another.pem"}
	err := c.Validate().(ValidationErrors)
	if !err.Has("security.custom_ca_files[1]") {
		t.Errorf("expected blank custom_ca_files entry to error, got %v", err)
	}
}

// --- Validate: feature flags -----------------------------------------------

func TestConfig_Validate_UnknownFeatureFlag_SilentlyIgnored(t *testing.T) {
	c := defaults()
	c.FeatureFlags["totally_new_flag_from_the_future"] = true
	c.FeatureFlags["another_unknown"] = false
	if err := c.Validate(); err != nil {
		t.Errorf("unknown feature flags must not error, got %v", err)
	}
}

func TestConfig_Validate_FeatureFlagEmptyKey(t *testing.T) {
	c := defaults()
	c.FeatureFlags[""] = true
	err := c.Validate().(ValidationErrors)
	if !err.Has("feature_flags") {
		t.Errorf("expected feature_flags error for empty key, got %v", err)
	}
}

// --- Validate: aggregation -------------------------------------------------

func TestConfig_Validate_AggregatesErrors(t *testing.T) {
	c := defaults()
	c.UI.AutoLockMinutes = -1          // err
	c.UI.Theme = "rainbow"             // err
	c.Network.HeartbeatSeconds = 99999 // err
	c.Telemetry.SampleRate = 2.0       // err
	c.Endpoints = []Endpoint{
		{ID: "", URL: "http://nope.example.com"}, // 2 errs (id, url)
		{ID: "dup", URL: "https://a.example.com"},
		{ID: "dup", URL: "not-a-url"}, // 2 errs (dup id, bad url)
	}

	err := c.Validate()
	if err == nil {
		t.Fatalf("expected aggregated errors, got nil")
	}
	ve, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("expected ValidationErrors, got %T", err)
	}
	// We expect at least 8 distinct ValidationError entries.
	if len(ve) < 7 {
		t.Errorf("expected >=7 aggregated errors, got %d: %v", len(ve), ve)
	}
	wantPaths := []string{
		"ui.auto_lock_minutes",
		"ui.theme",
		"network.heartbeat_seconds",
		"telemetry.sample_rate",
		"endpoints[0].id",
		"endpoints[0].url",
		"endpoints[2].id",
		"endpoints[2].url",
	}
	for _, p := range wantPaths {
		if !ve.Has(p) {
			t.Errorf("missing aggregated error for %q in %v", p, ve)
		}
	}
}

// --- Version / migration ---------------------------------------------------

func TestConfig_Validate_VersionTooNew(t *testing.T) {
	c := defaults()
	c.Version = currentVersion + 5
	err := c.Validate().(ValidationErrors)
	if !err.Has("version") {
		t.Errorf("expected version error, got %v", err)
	}
}

func TestConfig_Validate_NegativeVersion(t *testing.T) {
	c := defaults()
	c.Version = -1
	err := c.Validate().(ValidationErrors)
	if !err.Has("version") {
		t.Errorf("expected version error, got %v", err)
	}
}

func TestConfig_Load_MigratesOIDCToCookie(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Hand-write a config with the retired "oidc" method. We can't use
	// validConfig() since AuthKindOIDC was removed.
	raw := `{
		"version": 1,
		"endpoints": [
			{"id": "old", "label": "Old", "plugin_id": "ahv",
			 "url": "https://prism.example.com",
			 "auth_methods": ["basic", "oidc"]}
		]
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	m, err := Load(path, discardLogger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := m.Get()
	if len(got.Endpoints) != 1 {
		t.Fatalf("Endpoints: got %d want 1", len(got.Endpoints))
	}
	methods := got.Endpoints[0].AuthMethods
	// Both "oidc" (renamed) and "basic" (retired) fold into cookie, deduped.
	if len(methods) != 1 || methods[0] != AuthKindCookie {
		t.Errorf("expected [cookie] after migration, got %+v", methods)
	}
}

// --- ValidationError / ValidationErrors plumbing ---------------------------

func TestValidationError_ErrorString(t *testing.T) {
	e := ValidationError{Path: "ui.theme", Msg: "bad"}
	if got := e.Error(); got != "ui.theme: bad" {
		t.Errorf("Error(): got %q want %q", got, "ui.theme: bad")
	}
	e2 := ValidationError{Msg: "lone"}
	if got := e2.Error(); got != "lone" {
		t.Errorf("Error() with empty path: got %q want %q", got, "lone")
	}
}

func TestValidationErrors_ErrorString(t *testing.T) {
	var zero ValidationErrors
	if got := zero.Error(); got != "" {
		t.Errorf("empty ValidationErrors should return empty string, got %q", got)
	}
	one := ValidationErrors{{Path: "a", Msg: "b"}}
	if got := one.Error(); got != "a: b" {
		t.Errorf("single-error string: got %q", got)
	}
	many := ValidationErrors{{Path: "a", Msg: "b"}, {Path: "c", Msg: "d"}}
	got := many.Error()
	if !strings.Contains(got, "a: b") || !strings.Contains(got, "c: d") {
		t.Errorf("multi-error string should contain both: got %q", got)
	}
	if !strings.Contains(got, "2 problems") {
		t.Errorf("multi-error string should mention count: got %q", got)
	}
}

func TestValidationErrors_IsAnError(t *testing.T) {
	// Compile-time check that ValidationErrors satisfies the error interface.
	var _ error = ValidationErrors{}
	// And ValidationError.
	var _ error = ValidationError{}
}

// --- Endpoint.Allows -------------------------------------------------------

// --- TD-034: per-endpoint validation enforced from UpsertEndpoint -----------

func TestEndpoint_Validate_Valid(t *testing.T) {
	e := Endpoint{
		ID:       "prod",
		URL:      "https://prism.example.com",
		PluginID: "ahv",
	}
	if err := e.Validate(); err != nil {
		t.Errorf("valid endpoint rejected: %v", err)
	}
}

func TestEndpoint_Validate_RejectsHTTP(t *testing.T) {
	e := Endpoint{ID: "x", URL: "http://prism.example.com", PluginID: "ahv"}
	err := e.Validate()
	if err == nil {
		t.Fatalf("Endpoint.Validate accepted http:// URL")
	}
	var verrs ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("expected ValidationErrors wrap, got %T", err)
	}
}

func TestEndpoint_Validate_RejectsEmptyID(t *testing.T) {
	e := Endpoint{URL: "https://prism.example.com", PluginID: "ahv"}
	if err := e.Validate(); err == nil {
		t.Errorf("empty ID accepted")
	}
}

func TestEndpoint_Validate_RejectsMalformedPinnedCert(t *testing.T) {
	e := Endpoint{
		ID:            "p",
		URL:           "https://example.com",
		PluginID:      "ahv",
		PinnedCertB64: "not-base64!",
	}
	if err := e.Validate(); err == nil {
		t.Errorf("malformed pinned_cert_b64 accepted")
	}
}

func TestManager_UpsertEndpoint_RejectsHTTPURL(t *testing.T) {
	m := newTempManager(t)
	_, err := m.UpsertEndpoint(Endpoint{
		ID:       "x",
		URL:      "http://prism.example.com",
		PluginID: "ahv",
	})
	if err == nil {
		t.Fatalf("UpsertEndpoint silently accepted http://")
	}
}

func TestManager_UpsertEndpoint_RejectsBadPinnedCert(t *testing.T) {
	m := newTempManager(t)
	_, err := m.UpsertEndpoint(Endpoint{
		ID:            "x",
		URL:           "https://prism.example.com",
		PluginID:      "ahv",
		PinnedCertB64: "AAAA", // valid base64 but only 3 decoded bytes
	})
	if err == nil {
		t.Fatalf("UpsertEndpoint silently accepted short pinned cert")
	}
}

func TestManager_UpsertEndpoint_AcceptsValid(t *testing.T) {
	m := newTempManager(t)
	stored, err := m.UpsertEndpoint(Endpoint{
		ID:       "prod",
		URL:      "https://prism.example.com",
		PluginID: "ahv",
		Label:    "Production",
	})
	if err != nil {
		t.Fatalf("UpsertEndpoint rejected valid endpoint: %v", err)
	}
	if stored.ID != "prod" {
		t.Errorf("stored ID = %q, want prod", stored.ID)
	}
}

// newTempManager spins a Manager backed by a temp file. Used by the
// UpsertEndpoint tests above.
func newTempManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	m, err := Load(path, discardLogger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

func TestEndpoint_Allows(t *testing.T) {
	// Empty AuthMethods => cookie permitted (the only shipped method).
	e := Endpoint{}
	if !e.Allows(AuthKindCookie) {
		t.Errorf("empty endpoint should allow cookie")
	}
	if e.Allows(AuthKind("oidc")) {
		t.Errorf("empty endpoint should not allow oidc")
	}
	if e.Allows(AuthKind("basic")) {
		t.Errorf("empty endpoint should not allow retired basic")
	}

	e2 := Endpoint{AuthMethods: []AuthKind{AuthKindCookie}}
	if !e2.Allows(AuthKindCookie) {
		t.Errorf("cookie endpoint should allow cookie")
	}
	if e2.Allows(AuthKind("basic")) {
		t.Errorf("cookie-only endpoint should not allow basic")
	}
}

// --- Save / Update round-trip ----------------------------------------------

func TestManager_SaveAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	m, err := Load(path, discardLogger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := m.Update(func(c *Config) {
		c.UI.AutoLockMinutes = 17
		c.Network.HeartbeatSeconds = 42
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	m2, err := Load(path, discardLogger())
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	got := m2.Get()
	if got.UI.AutoLockMinutes != 17 {
		t.Errorf("AutoLockMinutes: got %d want 17", got.UI.AutoLockMinutes)
	}
	if got.Network.HeartbeatSeconds != 42 {
		t.Errorf("HeartbeatSeconds: got %d want 42", got.Network.HeartbeatSeconds)
	}
}

func TestManager_UpsertEndpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	m, err := Load(path, discardLogger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	saved, err := m.UpsertEndpoint(Endpoint{
		Label:    "My Cluster",
		PluginID: "ahv",
		URL:      "https://prism.example.com",
	})
	if err != nil {
		t.Fatalf("UpsertEndpoint: %v", err)
	}
	if saved.ID != "my-cluster" {
		t.Errorf("derived ID: got %q want my-cluster", saved.ID)
	}
	// AuthMethods should default to cookie-only (SSO).
	if len(saved.AuthMethods) != 1 || saved.AuthMethods[0] != AuthKindCookie {
		t.Errorf("AuthMethods defaults: got %+v want [cookie]", saved.AuthMethods)
	}

	// UpsertEndpoint with no URL must fail.
	if _, err := m.UpsertEndpoint(Endpoint{Label: "no url"}); err == nil {
		t.Errorf("expected error for missing URL")
	}
	if _, err := m.UpsertEndpoint(Endpoint{URL: "https://x", Label: "no plugin"}); err == nil {
		t.Errorf("expected error for missing plugin_id")
	}
}

func TestManager_DeleteEndpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	m, err := Load(path, discardLogger())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := m.UpsertEndpoint(Endpoint{Label: "a", PluginID: "ahv", URL: "https://a.example.com"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := m.DeleteEndpoint("a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := m.Endpoint("a"); ok {
		t.Errorf("Endpoint(a) still present after delete")
	}
	if err := m.DeleteEndpoint("nope"); err == nil {
		t.Errorf("expected error deleting unknown endpoint")
	}
}

// --- Compile-time interface assertion --------------------------------------

func TestValidationErrors_Unwrapping(t *testing.T) {
	// Sanity check that errors.As works through the standard error wrapper
	// for callers that want to type-assert.
	c := Config{Endpoints: []Endpoint{{ID: "x", URL: "http://no"}}}
	err := c.Validate()
	var ve ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As: expected match for ValidationErrors, got %T", err)
	}
	if len(ve) == 0 {
		t.Errorf("expected at least one validation error")
	}
}
