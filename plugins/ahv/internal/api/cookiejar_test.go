package api

import (
	"fmt"
	"testing"
)

func TestCookieJar_HeaderVerbatim(t *testing.T) {
	j := NewCookieJar([]SessionCookie{
		{Name: "NTNX_MERCURY_IAM_SESSION", Value: "abc=="}, // base64 padding preserved
		{Name: "_loginUser", Value: `"alice"`},             // double quotes preserved
	}, nil)
	got := j.Header()
	want := `NTNX_MERCURY_IAM_SESSION=abc==; _loginUser="alice"`
	if got != want {
		t.Errorf("Header verbatim:\n got %q\nwant %q", got, want)
	}
}

func TestCookieJar_RotateUpdatesAddsAndPersists(t *testing.T) {
	var persisted []SessionCookie
	calls := 0
	j := NewCookieJar([]SessionCookie{
		{Name: "NTNX_MERCURY_IAM_SESSION", Value: "old"},
		{Name: "keep", Value: "v"},
	}, func(snap []SessionCookie) {
		calls++
		persisted = snap
	})
	// Mercury IAM rotates the session cookie and adds a new one via Set-Cookie;
	// "keep" is not re-set and must survive.
	j.rotate([]string{
		`NTNX_MERCURY_IAM_SESSION=new; Path=/; HttpOnly; Secure`,
		`NTNX_IAM_SESSION="Ch=="; Path=/`,
	})
	if calls != 1 {
		t.Fatalf("onRotate calls: got %d want 1", calls)
	}
	got := j.Header()
	want := `NTNX_MERCURY_IAM_SESSION=new; keep=v; NTNX_IAM_SESSION="Ch=="`
	if got != want {
		t.Errorf("Header after rotate:\n got %q\nwant %q", got, want)
	}
	if len(persisted) != 3 {
		t.Errorf("persisted snapshot len: got %d want 3 (%+v)", len(persisted), persisted)
	}
}

func TestCookieJar_RotateNoChangeNoCallback(t *testing.T) {
	calls := 0
	j := NewCookieJar([]SessionCookie{{Name: "s", Value: "same"}}, func([]SessionCookie) { calls++ })
	j.rotate([]string{`s=same; Path=/`}) // identical value — must be a no-op
	if calls != 0 {
		t.Errorf("onRotate should not fire when nothing changed; got %d calls", calls)
	}
	if got := j.Header(); got != "s=same" {
		t.Errorf("Header unchanged: got %q", got)
	}
}

func TestParseSetCookie(t *testing.T) {
	cases := []struct {
		in          string
		name, value string
		ok          bool
	}{
		{`NTNX_MERCURY_IAM_SESSION=abc==; Path=/; HttpOnly`, "NTNX_MERCURY_IAM_SESSION", "abc==", true},
		{`_loginUser="alice"; Secure`, "_loginUser", `"alice"`, true},
		{`bare=value`, "bare", "value", true},
		{`novalue`, "", "", false},
		{`  spaced = v ; Path=/`, "spaced", "v", true},
	}
	for _, c := range cases {
		n, v, ok := parseSetCookie(c.in)
		if ok != c.ok || n != c.name || v != c.value {
			t.Errorf("parseSetCookie(%q) = (%q,%q,%v); want (%q,%q,%v)", c.in, n, v, ok, c.name, c.value, c.ok)
		}
	}
}

// TestCookieJar_RotatePublicWrapper (NUTA-05): the exported Rotate helper drives
// the same name-keyed merge + persist as the internal rotate, so the VNC WS
// upgrade response's Set-Cookie rotation lands in the jar and the keystore. The
// WS response never flows through the REST cookie-capture transport, so this is
// the only place that rotation gets captured.
func TestCookieJar_RotatePublicWrapper(t *testing.T) {
	var persisted []SessionCookie
	calls := 0
	j := NewCookieJar([]SessionCookie{
		{Name: "NTNX_MERCURY_IAM_SESSION", Value: "old"},
	}, func(snap []SessionCookie) {
		calls++
		persisted = snap
	})
	// Simulate the WS upgrade response rotating the session cookie.
	j.Rotate([]string{`NTNX_MERCURY_IAM_SESSION=ws-rotated; Path=/; HttpOnly; Secure`})
	if calls != 1 {
		t.Fatalf("Rotate must fire onRotate exactly once on change, got %d", calls)
	}
	if got := j.Header(); got != "NTNX_MERCURY_IAM_SESSION=ws-rotated" {
		t.Errorf("jar did not pick up WS Set-Cookie rotation: got %q", got)
	}
	if len(persisted) != 1 || persisted[0].Value != "ws-rotated" {
		t.Errorf("rotated cookie not persisted via onRotate: %+v", persisted)
	}
	// A nil/empty slice (no Set-Cookie on the upgrade) must be a safe no-op.
	j.Rotate(nil)
	if calls != 1 {
		t.Errorf("empty Rotate must not fire onRotate, calls now %d", calls)
	}
}

// TestClient_RotateCookies_NoJarNoPanic (NUTA-05): RotateCookies on a client
// without a jar (the unauthenticated health probe client) is a safe no-op.
func TestClient_RotateCookies_NoJarNoPanic(t *testing.T) {
	c, err := NewClient(ClientOptions{BaseURL: "https://example.invalid"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	c.RotateCookies([]string{"x=y"}) // must not panic
}

func TestCookieJar_RotateCapsUnboundedGrowth(t *testing.T) {
	// A compromised/MITM'd upstream that returns a fresh distinct
	// Set-Cookie name on every response must not grow the jar without bound.
	j := NewCookieJar(nil, nil)
	// Feed far more distinct names than the cap allows, one per rotate (as the
	// 30s keep-alive would over time).
	for i := 0; i < maxJarCookies*3; i++ {
		j.rotate([]string{fmt.Sprintf("x%d=junk; Path=/", i)})
	}
	if got := len(j.cookies); got != maxJarCookies {
		t.Fatalf("jar grew to %d cookies; want capped at %d", got, maxJarCookies)
	}
	// A legitimate rotation of an ALREADY-known cookie must still apply even
	// once the cap is reached (update-in-place is unbounded-safe).
	j.rotate([]string{"x0=rotated; Path=/"})
	for _, c := range j.cookies {
		if c.Name == "x0" {
			if c.Value != "rotated" {
				t.Fatalf("x0 value = %q; want rotated (in-place update must survive the cap)", c.Value)
			}
			return
		}
	}
	t.Fatal("x0 not found after cap")
}
