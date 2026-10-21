package ahv

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

func connState(certs ...*x509.Certificate) tls.ConnectionState {
	return tls.ConnectionState{PeerCertificates: certs}
}

// makeSelfSigned returns a fresh ECDSA leaf cert + its DER bytes,
// with the given hostname in SAN. Used to drive the pin verifier.
func makeSelfSigned(t *testing.T, host string) ([]byte, *x509.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return der, cert
}

// newDiscardLogger returns a slog that throws everything away — we
// don't care about test-side logging.
func newDiscardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// testLogger implements sdk.Logger by forwarding to slog.
type testLogger struct{ *slog.Logger }

func (l testLogger) Debug(msg string, args ...any) { l.Logger.Debug(msg, args...) }
func (l testLogger) Info(msg string, args ...any)  { l.Logger.Info(msg, args...) }
func (l testLogger) Warn(msg string, args ...any)  { l.Logger.Warn(msg, args...) }
func (l testLogger) Error(msg string, args ...any) { l.Logger.Error(msg, args...) }
func (l testLogger) With(args ...any) sdk.Logger {
	return testLogger{l.Logger.With(args...)}
}

func TestPinSPKI_acceptsMatch(t *testing.T) {
	der, cert := makeSelfSigned(t, "prism.local")
	p := &Plugin{deps: sdk.Dependencies{Logger: testLogger{newDiscardLogger()}}}
	cfg := p.tlsConfigFor(sdk.EndpointRef{
		ID:            "test",
		URL:           "https://prism.local",
		PinnedCertB64: base64.StdEncoding.EncodeToString(der),
	})
	if cfg.VerifyConnection == nil {
		t.Fatalf("expected VerifyConnection to be set with a pin")
	}
	if err := cfg.VerifyConnection(connState(cert)); err != nil {
		t.Errorf("expected accept on match, got %v", err)
	}
	_ = der
}

func TestPinSPKI_rejectsMismatch(t *testing.T) {
	der1, _ := makeSelfSigned(t, "prism.local")
	_, cert2 := makeSelfSigned(t, "prism.local")
	p := &Plugin{deps: sdk.Dependencies{Logger: testLogger{newDiscardLogger()}}}
	cfg := p.tlsConfigFor(sdk.EndpointRef{
		ID:            "test",
		URL:           "https://prism.local",
		PinnedCertB64: base64.StdEncoding.EncodeToString(der1),
	})
	err := cfg.VerifyConnection(connState(cert2))
	if err == nil || !strings.Contains(err.Error(), "SPKI mismatch") {
		t.Errorf("expected SPKI mismatch error, got %v", err)
	}
}

func TestPinSPKI_survivesCertRotationWithSameKey(t *testing.T) {
	// Same key, different cert (e.g. renewed before expiry). SPKI pin
	// should still match because SPKI = hash(SubjectPublicKeyInfo) and
	// the key didn't change. DER pin would have rejected.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	mkCert := func(serial int64, notAfter time.Duration) ([]byte, *x509.Certificate) {
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: "prism.local"},
			DNSNames:     []string{"prism.local"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(notAfter),
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
		if err != nil {
			t.Fatalf("cert: %v", err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return der, c
	}
	derOld, _ := mkCert(1, time.Hour)
	derNew, certNew := mkCert(2, 24*time.Hour) // same key, different serial+expiry
	p := &Plugin{deps: sdk.Dependencies{Logger: testLogger{newDiscardLogger()}}}
	cfg := p.tlsConfigFor(sdk.EndpointRef{
		ID:            "test",
		URL:           "https://prism.local",
		PinnedCertB64: base64.StdEncoding.EncodeToString(derOld),
	})
	if err := cfg.VerifyConnection(connState(certNew)); err != nil {
		t.Errorf("SPKI pin should survive rotation with same key: %v", err)
	}
	if sha256.Sum256(derOld) == sha256.Sum256(derNew) {
		t.Fatalf("test setup: derOld and derNew are identical")
	}
}

func TestPinFailClosed_onUnparseablePin(t *testing.T) {
	p := &Plugin{deps: sdk.Dependencies{Logger: testLogger{newDiscardLogger()}}}
	cfg := p.tlsConfigFor(sdk.EndpointRef{
		ID:            "test",
		URL:           "https://prism.local",
		PinnedCertB64: "this-is-not-base64!!!",
	})
	if cfg.VerifyConnection == nil {
		t.Fatalf("expected fail-closed verifier even with broken pin")
	}
	_, cert := makeSelfSigned(t, "prism.local")
	if err := cfg.VerifyConnection(connState(cert)); err == nil {
		t.Errorf("expected fail-closed on broken pin, got accept")
	}
}

func TestFailClosed_onAllowInsecureWithoutPin(t *testing.T) {
	// allow_insecure_tls with no pin must NOT silently disable verification.
	// config.Validate accepts this combination (an endpoint awaiting trust on
	// first use), so this check is the safeguard: tlsConfigFor refuses the
	// connection rather than trusting any cert until a pin is stored.
	p := &Plugin{deps: sdk.Dependencies{Logger: testLogger{newDiscardLogger()}}}
	cfg := p.tlsConfigFor(sdk.EndpointRef{
		ID:               "test",
		URL:              "https://prism.local",
		AllowInsecureTLS: true,
		// no PinnedCertB64
	})
	if cfg.VerifyConnection == nil {
		t.Fatalf("expected a fail-closed verifier, not InsecureSkipVerify=true with no verifier")
	}
	_, cert := makeSelfSigned(t, "prism.local")
	if err := cfg.VerifyConnection(connState(cert)); err == nil {
		t.Errorf("expected allow_insecure_tls-without-pin to be refused, got accept")
	}
}

func TestPinRejectsExpiredCert(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "prism.local"},
		DNSNames:     []string{"prism.local"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour), // expired yesterday
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	p := &Plugin{deps: sdk.Dependencies{Logger: testLogger{newDiscardLogger()}}}
	cfg := p.tlsConfigFor(sdk.EndpointRef{
		ID:            "test",
		URL:           "https://prism.local",
		PinnedCertB64: base64.StdEncoding.EncodeToString(der),
	})
	err = cfg.VerifyConnection(connState(cert))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected expiry rejection, got %v", err)
	}
}

func TestPinRejectsHostnameMismatch(t *testing.T) {
	der, cert := makeSelfSigned(t, "actual.local")
	p := &Plugin{deps: sdk.Dependencies{Logger: testLogger{newDiscardLogger()}}}
	cfg := p.tlsConfigFor(sdk.EndpointRef{
		ID:            "test",
		URL:           "https://expected.local",
		PinnedCertB64: base64.StdEncoding.EncodeToString(der),
	})
	err := cfg.VerifyConnection(connState(cert))
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Errorf("expected hostname mismatch, got %v", err)
	}
}
