package ipc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHMACAuthRejectsMissingSignature verifies the middleware rejects
// unsigned requests outside of the /healthz and /version allow-list.
func TestHMACAuthRejectsMissingSignature(t *testing.T) {
	secret := NewSecret([]byte("0123456789abcdef0123456789abcdef")) // 32 bytes
	defer secret.Wipe()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	handler := hmacAuth(secret, nil, next)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHMACAuthAcceptsSignedRequest(t *testing.T) {
	secret := NewSecret([]byte("0123456789abcdef0123456789abcdef")) // 32 bytes

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	handler := hmacAuth(secret, nil, next)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	body := []byte(`{"hello":"world"}`)
	bodyHash := sha256.Sum256(body)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "abcdef0123456789"
	uri := "/v1/plugins/foo/auth/start"
	canonical := strings.Join([]string{"POST", uri, ts, nonce, hex.EncodeToString(bodyHash[:])}, "\n")
	sig := hex.EncodeToString(secret.MAC([]byte(canonical)))

	req, _ := http.NewRequest(http.MethodPost, srv.URL+uri, bytes.NewReader(body))
	req.Header.Set("X-HC-Sig", sig)
	req.Header.Set("X-HC-Timestamp", ts)
	req.Header.Set("X-HC-Nonce", nonce)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(raw))
	}
}

func TestHealthzBypassesAuth(t *testing.T) {
	secret := NewSecret([]byte("0123456789abcdef0123456789abcdef"))
	defer secret.Wipe()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := hmacAuth(secret, nil, next)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz should bypass auth, got %d", resp.StatusCode)
	}
}
