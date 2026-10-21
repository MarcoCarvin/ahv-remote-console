package ahv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

// recordingKeystore captures the last persisted value per key so a test can
// assert what a rotation wrote.
type recordingKeystore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newRecordingKeystore() *recordingKeystore {
	return &recordingKeystore{data: map[string][]byte{}}
}

func (k *recordingKeystore) Get(key string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.data[key]
	if !ok {
		return nil, http.ErrNoCookie
	}
	return v, nil
}

func (k *recordingKeystore) Set(key string, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.data[key] = append([]byte(nil), value...)
	return nil
}

func (k *recordingKeystore) Delete(key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.data, key)
	return nil
}

func newRotationPlugin(eps []sdk.EndpointRef, ks sdk.Keystore) *Plugin {
	return &Plugin{
		deps: sdk.Dependencies{
			Logger:   testLogger{newDiscardLogger()},
			Keystore: ks,
			Config:   stubConfig{eps: eps},
		},
		state:    map[string]*endpointState{},
		clients:  map[string]*api.Client{},
		sessions: map[string]*sessionState{},
		cache:    newResourceCache(time.Second),
	}
}

// TestOnRotate_SyncsInMemoryState (NUTA-03): a Set-Cookie rotation captured on a
// live request must update BOTH the keystore AND the in-memory endpointState. If
// only the keystore is updated, an in-process client rebuild (clientFor →
// newCookieClient) reseeds the jar from the stale in-memory cookies and undoes
// the rotation — the "constant reconnecting" root cause.
func TestOnRotate_SyncsInMemoryState(t *testing.T) {
	// A server that authenticates fine and rotates the session cookie.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "NTNX_MERCURY_IAM_SESSION=rotated-value; Path=/; HttpOnly")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ep := sdk.EndpointRef{
		ID:          "ep1",
		URL:         srv.URL,
		PluginID:    pluginID,
		AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie},
	}
	ks := newRecordingKeystore()
	p := newRotationPlugin([]sdk.EndpointRef{ep}, ks)

	initial := []api.SessionCookie{{Name: "NTNX_MERCURY_IAM_SESSION", Value: "initial-value"}}
	st := &endpointState{method: sdk.AuthKindCookie, sessionCookies: initial}
	client, err := p.newCookieClient(ep, initial)
	if err != nil {
		t.Fatalf("newCookieClient: %v", err)
	}
	// Register the client + state so the onRotate closure can find the state.
	p.replaceClient(ep.ID, client, st)

	// Drive a request so the cookie-capture transport sees the Set-Cookie and
	// fires onRotate.
	if err := client.VerifyAuth(context.Background()); err != nil {
		t.Fatalf("VerifyAuth: %v", err)
	}

	// In-memory state must now carry the rotated value (NUTA-03).
	p.mu.RLock()
	got := p.state[ep.ID].sessionCookies
	p.mu.RUnlock()
	if len(got) != 1 || got[0].Value != "rotated-value" {
		t.Errorf("in-memory state not synced on rotation: %+v", got)
	}

	// Keystore must also carry the rotation (pre-existing behavior, regression guard).
	raw, err := ks.Get(keySSOCookies(ep.ID))
	if err != nil {
		t.Fatalf("keystore not written: %v", err)
	}
	if got := string(raw); !strings.Contains(got, "rotated-value") {
		t.Errorf("keystore did not persist rotation: %s", got)
	}
}

// TestAuthStartCookie_PersistsRotatedCookies: after a cookie sign-in, both the
// keystore AND in-memory state must carry the POST-rotation session cookie, not
// the captured pre-rotation one. VerifyAuth triggers Prism Mercury's first
// rotation; persisting the captured set would clobber the rotated keychain entry
// and seed a dead cookie, so a later cold rehydrate (runtime restart) 401s —
// the cold-path half of the re-auth loop.
func TestAuthStartCookie_PersistsRotatedCookies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "NTNX_MERCURY_IAM_SESSION=rotated-value; Path=/; HttpOnly")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ep := sdk.EndpointRef{
		ID:          "ep1",
		URL:         srv.URL,
		PluginID:    pluginID,
		AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie},
	}
	ks := newRecordingKeystore()
	p := newRotationPlugin([]sdk.EndpointRef{ep}, ks)

	captured := []api.SessionCookie{{Name: "NTNX_MERCURY_IAM_SESSION", Value: "captured-value"}}
	if _, err := p.authStartCookie(
		context.Background(), ep,
		authStartBody{Method: sdk.AuthKindCookie, Cookies: captured},
	); err != nil {
		t.Fatalf("authStartCookie: %v", err)
	}

	// In-memory state must carry the ROTATED value, not the captured one.
	p.mu.RLock()
	got := p.state[ep.ID].sessionCookies
	p.mu.RUnlock()
	if len(got) != 1 || got[0].Value != "rotated-value" {
		t.Errorf("state must hold the rotated cookie, got: %+v", got)
	}
	// Keystore must hold ROTATED only — never the captured pre-rotation value
	// (which the clobber bug persisted last).
	raw, err := ks.Get(keySSOCookies(ep.ID))
	if err != nil {
		t.Fatalf("keystore not written: %v", err)
	}
	if s := string(raw); !strings.Contains(s, "rotated-value") || strings.Contains(s, "captured-value") {
		t.Errorf("keystore must hold the rotated cookie only, got: %s", s)
	}
}

// TestKeepAliveTick_InvalidatesOnAuthFailure (NUTA-07): a keep-alive ping that
// gets a 401 must proactively invalidate the endpoint (drop the cached client +
// persisted cookies), not merely Debug-log, so the dead session is surfaced at
// once instead of lingering until the next API call.
func TestKeepAliveTick_InvalidatesOnAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"session expired"}`))
	}))
	defer srv.Close()

	ep := sdk.EndpointRef{
		ID:          "ep1",
		URL:         srv.URL,
		PluginID:    pluginID,
		AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie},
	}
	ks := newRecordingKeystore()
	p := newRotationPlugin([]sdk.EndpointRef{ep}, ks)

	cookies := []api.SessionCookie{{Name: "NTNX_MERCURY_IAM_SESSION", Value: "v"}}
	_ = ks.Set(keySSOCookies(ep.ID), []byte(`[{"Name":"NTNX_MERCURY_IAM_SESSION","Value":"v"}]`))
	client, err := p.newCookieClient(ep, cookies)
	if err != nil {
		t.Fatalf("newCookieClient: %v", err)
	}
	p.replaceClient(ep.ID, client, &endpointState{method: sdk.AuthKindCookie, sessionCookies: cookies})

	p.keepAliveTick(context.Background())

	// The endpoint must have been invalidated: cached client + state dropped,
	// persisted cookies deleted.
	p.mu.RLock()
	_, clientStill := p.clients[ep.ID]
	_, stateStill := p.state[ep.ID]
	p.mu.RUnlock()
	if clientStill {
		t.Error("expected cached client dropped after auth-failed keep-alive")
	}
	if stateStill {
		t.Error("expected endpoint state dropped after auth-failed keep-alive")
	}
	if _, err := ks.Get(keySSOCookies(ep.ID)); err == nil {
		t.Error("expected persisted cookies deleted after auth-failed keep-alive")
	}
}

// TestKeepAliveTick_KeepsSessionOn403: a 403 is per-resource RBAC / a transient
// WAF-LB hiccup on the probe path, NOT session expiry — the keep-alive must KEEP
// the session (only a 401 invalidates). Wiping on 403 would log the operator out
// of a perfectly live session.
func TestKeepAliveTick_KeepsSessionOn403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()

	ep := sdk.EndpointRef{
		ID:          "ep1",
		URL:         srv.URL,
		PluginID:    pluginID,
		AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie},
	}
	ks := newRecordingKeystore()
	p := newRotationPlugin([]sdk.EndpointRef{ep}, ks)

	cookies := []api.SessionCookie{{Name: "NTNX_MERCURY_IAM_SESSION", Value: "v"}}
	_ = ks.Set(keySSOCookies(ep.ID), []byte(`[{"Name":"NTNX_MERCURY_IAM_SESSION","Value":"v"}]`))
	client, err := p.newCookieClient(ep, cookies)
	if err != nil {
		t.Fatalf("newCookieClient: %v", err)
	}
	p.replaceClient(ep.ID, client, &endpointState{method: sdk.AuthKindCookie, sessionCookies: cookies})

	p.keepAliveTick(context.Background())

	// The session must SURVIVE a 403: cached client, state, and persisted cookies
	// all intact.
	p.mu.RLock()
	_, clientStill := p.clients[ep.ID]
	_, stateStill := p.state[ep.ID]
	p.mu.RUnlock()
	if !clientStill {
		t.Error("403 keep-alive must NOT drop the cached client")
	}
	if !stateStill {
		t.Error("403 keep-alive must NOT drop the endpoint state")
	}
	if _, err := ks.Get(keySSOCookies(ep.ID)); err != nil {
		t.Error("403 keep-alive must NOT delete persisted cookies")
	}
}

// TestKeepAliveTick_KeepsHealthyEndpoint (NUTA-07): a successful ping must NOT
// invalidate the endpoint — the session is alive.
func TestKeepAliveTick_KeepsHealthyEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ep := sdk.EndpointRef{
		ID:          "ep1",
		URL:         srv.URL,
		PluginID:    pluginID,
		AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie},
	}
	p := newRotationPlugin([]sdk.EndpointRef{ep}, newRecordingKeystore())
	cookies := []api.SessionCookie{{Name: "s", Value: "v"}}
	client, err := p.newCookieClient(ep, cookies)
	if err != nil {
		t.Fatalf("newCookieClient: %v", err)
	}
	p.replaceClient(ep.ID, client, &endpointState{method: sdk.AuthKindCookie, sessionCookies: cookies})

	p.keepAliveTick(context.Background())

	p.mu.RLock()
	_, clientStill := p.clients[ep.ID]
	p.mu.RUnlock()
	if !clientStill {
		t.Error("a healthy keep-alive must not invalidate the endpoint")
	}
}

// TestInvalidateEndpoint_SignalsSessionsForReauth (SH2): when an endpoint's
// credentials go invalid, the plugin reports exactly the sessions ON THAT
// endpoint to the runtime (Dependencies.OnSessionsExpired) so their consoles can
// proactively enter the re-auth arm — and sessions on other endpoints are left
// alone. Sessions themselves must NOT be deleted (a re-auth reconnects them).
func TestInvalidateEndpoint_SignalsSessionsForReauth(t *testing.T) {
	p := newRotationPlugin(nil, newRecordingKeystore())
	var got []string
	p.deps.OnSessionsExpired = func(ids []string) { got = append(got, ids...) }
	p.sessions["s1"] = &sessionState{EndpointID: "ep1"}
	p.sessions["s2"] = &sessionState{EndpointID: "ep1"}
	p.sessions["other"] = &sessionState{EndpointID: "ep2"}

	p.invalidateEndpoint("ep1")

	set := map[string]bool{}
	for _, id := range got {
		set[id] = true
	}
	if len(got) != 2 || !set["s1"] || !set["s2"] {
		t.Errorf("OnSessionsExpired = %v, want exactly {s1,s2}", got)
	}
	// Sessions are preserved — a re-auth refreshes cookies and reconnects them.
	p.mu.RLock()
	n := len(p.sessions)
	p.mu.RUnlock()
	if n != 3 {
		t.Errorf("sessions deleted by invalidate: have %d, want 3", n)
	}
}
