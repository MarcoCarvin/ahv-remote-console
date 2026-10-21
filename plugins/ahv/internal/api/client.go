// Package api wraps the Prism Central v3 endpoints used by the AHV plugin.
//
// Endpoints implemented:
//
//	POST /api/nutanix/v3/vms/list           — paginated VM list
//	GET  /api/nutanix/v3/vms/{uuid}         — VM detail (status, IPs)
//	POST /console/vnc_auto.html?uuid=…      — console ticket (legacy Prism)
//	POST /api/nutanix/v3/users/me           — identity probe
//
// The package is intentionally tight: only what the plugin needs. It does not
// attempt to be a general Prism SDK.
package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/google/uuid"
)

type ClientOptions struct {
	BaseURL       string
	TLSConfig     *tls.Config
	Logger        sdk.Logger
	Authenticator Authenticator
	// CookieJar, when set, takes precedence over Authenticator: the client
	// replays the jar's current cookies on every request AND captures
	// Set-Cookie rotation from every response. This is how we honor Mercury
	// IAM's server-side session refresh (it renews the session via the refresh
	// token and hands back a fresh cookie via Set-Cookie) instead of replaying
	// a fixed cookie until it dies on its absolute schedule.
	CookieJar *CookieJar
	// ProxyURL / NoProxy wire air-gap/egress-proxy configuration into BOTH the
	// REST transport and the VNC WebSocket dialer (OBS-02). When ProxyURL is
	// empty the client defaults to http.ProxyFromEnvironment so standard
	// HTTP_PROXY / HTTPS_PROXY / NO_PROXY env vars are honored out of the box.
	// NoProxy entries are merged into the env-var no-proxy semantics when an
	// explicit ProxyURL is set.
	ProxyURL string
	NoProxy  []string
}

// Authenticator sets the auth header(s) on every outbound request.
// The live implementation replays the Prism session cookies captured via
// the embedded SSO login (see CookieJar / ClientOptions.CookieJar).
type Authenticator interface {
	Apply(ctx context.Context, req *http.Request) error
}

// AuthenticatorFunc adapts a free function to the Authenticator interface.
type AuthenticatorFunc func(ctx context.Context, req *http.Request) error

func (f AuthenticatorFunc) Apply(ctx context.Context, req *http.Request) error {
	return f(ctx, req)
}

// SessionCookie names a single cookie captured from an embedded SSO login.
type SessionCookie struct {
	Name  string
	Value string
}

// CookieJar holds the live Prism session cookies and honors Set-Cookie
// rotation. Prism Central Mercury IAM refreshes the session server-side (using
// the refresh token cookie) and returns a fresh session cookie via Set-Cookie;
// replaying the originally-captured cookie forever lets the session expire on
// its absolute schedule, which is the root cause of the "constant reconnecting"
// the operator hit. The jar replays the LATEST cookies verbatim (Prism uses
// double-quoted values that http.Cookie.String() would mangle) and fires
// onRotate when a value changes so the plugin can persist the refreshed set.
//
// Safe for concurrent use: the request path reads via Header(); response
// captures merge via rotate().
type CookieJar struct {
	mu       sync.RWMutex
	cookies  []SessionCookie
	onRotate func([]SessionCookie)
}

// maxJarCookies bounds how many distinct cookies the jar will hold. Prism's
// live session set is well under a dozen; 64 is generous headroom for any
// legitimate Mercury IAM rotation while capping an attacker-driven growth DoS.
const maxJarCookies = 64

// NewCookieJar seeds the jar with the captured cookies. onRotate (may be nil)
// is invoked, outside the lock, with a fresh snapshot whenever a cookie value
// changes — the plugin uses it to persist the refreshed set to the keystore.
func NewCookieJar(initial []SessionCookie, onRotate func([]SessionCookie)) *CookieJar {
	return &CookieJar{cookies: append([]SessionCookie(nil), initial...), onRotate: onRotate}
}

// Header builds the verbatim "name=value; ..." Cookie header from the current
// set. Built by string concatenation (not http.Cookie.String(), which strips
// Prism's double-quoted values and gets the request rejected as "Invalid
// cookies present").
func (j *CookieJar) Header() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if len(j.cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(j.cookies))
	for _, c := range j.cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// Snapshot returns a copy of the jar's current cookie set. Used after an auth
// probe so the caller persists the POST-rotation cookies (Mercury IAM rotates
// the session on the first request) rather than the pre-rotation captured set.
func (j *CookieJar) Snapshot() []SessionCookie {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return append([]SessionCookie(nil), j.cookies...)
}

// Rotate feeds raw Set-Cookie header values into the jar from a code path
// outside the REST transport — specifically the VNC WebSocket upgrade response
// (NUTA-05), whose Set-Cookie rotation the http.Client transport never sees.
// It is a thin exported wrapper over rotate so the same name-keyed merge, cap,
// and onRotate-persist semantics apply. Safe to call with a nil or empty slice.
func (j *CookieJar) Rotate(setCookie []string) {
	j.rotate(setCookie)
}

// rotate merges Set-Cookie response headers into the live set by name (verbatim
// value), keeping any cookies Prism didn't re-set. Fires onRotate exactly once,
// outside the lock, if anything changed.
func (j *CookieJar) rotate(setCookie []string) {
	if len(setCookie) == 0 {
		return
	}
	j.mu.Lock()
	changed := false
	for _, sc := range setCookie {
		name, value, ok := parseSetCookie(sc)
		if !ok {
			continue
		}
		idx := -1
		for i := range j.cookies {
			if j.cookies[i].Name == name {
				idx = i
				break
			}
		}
		if idx >= 0 {
			// Updating an existing cookie in place is always safe — the set
			// size is bounded by what's already there.
			if j.cookies[idx].Value != value {
				j.cookies[idx].Value = value
				changed = true
			}
		} else if len(j.cookies) < maxJarCookies {
			j.cookies = append(j.cookies, SessionCookie{Name: name, Value: value})
			changed = true
		}
		// else: a never-seen name past the cap is dropped. A compromised or
		// transiently-MITM'd upstream can otherwise return a fresh distinct
		// Set-Cookie name on every response (the 30s keep-alive drives one
		// every 30s), growing the in-memory jar, the persisted keystore blob,
		// and the replayed Cookie header without bound until requests are
		// rejected on header size (session-kill). Prism's real session set is
		// well under the cap, so legitimate rotation (e.g. Mercury IAM adding
		// NTNX_IAM_SESSION) still lands.
	}
	snapshot := append([]SessionCookie(nil), j.cookies...)
	cb := j.onRotate
	j.mu.Unlock()
	if changed && cb != nil {
		cb(snapshot)
	}
}

// parseSetCookie extracts the verbatim name=value from a Set-Cookie header
// (the segment before the first ';', split on the first '='). Quotes and
// base64 '=' padding in the value are preserved.
func parseSetCookie(sc string) (name, value string, ok bool) {
	seg := sc
	if i := strings.IndexByte(sc, ';'); i >= 0 {
		seg = sc[:i]
	}
	eq := strings.IndexByte(seg, '=')
	if eq < 0 {
		return "", "", false
	}
	name = strings.TrimSpace(seg[:eq])
	value = strings.TrimSpace(seg[eq+1:])
	return name, value, name != ""
}

// cookieJarAuthenticator replays the jar's current cookies. Used internally
// when ClientOptions.CookieJar is set.
func cookieJarAuthenticator(jar *CookieJar) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, req *http.Request) error {
		if h := jar.Header(); h != "" {
			req.Header.Set("Cookie", h)
		}
		return nil
	})
}

// cookieCaptureTransport wraps a RoundTripper and feeds every response's
// Set-Cookie headers into the jar, so session rotation is captured on the live
// request path (the 30s /users/me keepalive drives it).
type cookieCaptureTransport struct {
	base http.RoundTripper
	jar  *CookieJar
}

func (t *cookieCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if resp != nil {
		t.jar.rotate(resp.Header.Values("Set-Cookie"))
	}
	return resp, err
}

type Client struct {
	base *url.URL
	http *http.Client
	// httpUpload is a separate client used for long-running body
	// uploads (e.g. PUT /images/{uuid}/file with a 12 MiB ISO over
	// a customer VPN). The shared `http` client has a hard 60s
	// timeout that's appropriate for the JSON control plane but
	// would prematurely kill any large upload. This one has no
	// client-level Timeout — cancellation is the request context's
	// responsibility — and a much longer ResponseHeaderTimeout so
	// Prism can take its time acknowledging the upload.
	httpUpload *http.Client
	log        sdk.Logger
	auth       Authenticator
	// jar is the live cookie set (non-nil only on the cookie/SSO path). The VNC
	// WS upgrade response's Set-Cookie rotation is fed back into it (NUTA-05).
	jar *CookieJar
	// proxy is the resolved proxy function shared by the REST transport and the
	// VNC WebSocket dialer so egress proxy / air-gap config (OBS-02) is honored
	// on both. Never nil — defaults to http.ProxyFromEnvironment.
	proxy func(*http.Request) (*url.URL, error)
	// v4mu guards the cached VMM-v4 capability probe (NUTA-01). v4Probed is set
	// only once the probe REACHED the PC (any HTTP response); a transport
	// failure leaves it false so the next call re-probes. v4Available holds the
	// cached answer once v4Probed is true.
	v4mu        sync.Mutex
	v4Probed    bool
	v4Available bool
}

func NewClient(opts ClientOptions) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("api: BaseURL required")
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse BaseURL: %w", err)
	}
	proxy, err := resolveProxy(opts.ProxyURL, opts.NoProxy)
	if err != nil {
		return nil, fmt.Errorf("resolve proxy: %w", err)
	}
	tr := &http.Transport{
		Proxy:                 proxy,
		TLSClientConfig:       opts.TLSConfig,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	uploadTr := &http.Transport{
		Proxy:                 proxy,
		TLSClientConfig:       opts.TLSConfig,
		MaxIdleConns:          4,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
	}
	var httpTransport http.RoundTripper = tr
	var uploadTransport http.RoundTripper = uploadTr
	auth := opts.Authenticator
	if opts.CookieJar != nil {
		// The jar drives both the replayed Cookie header (auth) and the
		// captured Set-Cookie rotation (transport) — covering REST and, via
		// WSDialHeader's c.auth.Apply, the VNC WebSocket upgrade.
		auth = cookieJarAuthenticator(opts.CookieJar)
		httpTransport = &cookieCaptureTransport{base: tr, jar: opts.CookieJar}
		uploadTransport = &cookieCaptureTransport{base: uploadTr, jar: opts.CookieJar}
	}
	return &Client{
		base: u,
		http: &http.Client{
			Transport: httpTransport,
			Timeout:   60 * time.Second,
		},
		httpUpload: &http.Client{
			Transport: uploadTransport,
			// No Timeout: rely on the request's context.Context for
			// cancellation. MountAgentISO sets a 10-minute upload
			// deadline; tests use shorter context windows.
		},
		log:   opts.Logger,
		auth:  auth,
		jar:   opts.CookieJar,
		proxy: proxy,
	}, nil
}

// resolveProxy builds the proxy function shared by the REST transport and the
// VNC WebSocket dialer (OBS-02). When proxyURL is empty it returns
// http.ProxyFromEnvironment so standard HTTP_PROXY / HTTPS_PROXY / NO_PROXY env
// vars work in air-gapped / proxied deployments with zero config. When an
// explicit proxyURL is set, that URL is used for every request EXCEPT hosts
// matching a noProxy entry (suffix match on host or host:port), which dial
// directly.
func resolveProxy(proxyURL string, noProxy []string) (func(*http.Request) (*url.URL, error), error) {
	if proxyURL == "" {
		return http.ProxyFromEnvironment, nil
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url %q: %w", proxyURL, err)
	}
	bypass := make([]string, 0, len(noProxy))
	for _, n := range noProxy {
		n = strings.TrimSpace(n)
		if n != "" {
			bypass = append(bypass, strings.ToLower(n))
		}
	}
	return func(req *http.Request) (*url.URL, error) {
		host := strings.ToLower(req.URL.Hostname())
		hostPort := strings.ToLower(req.URL.Host)
		for _, b := range bypass {
			if host == b || hostPort == b ||
				strings.HasSuffix(host, "."+strings.TrimPrefix(b, ".")) {
				return nil, nil // bypass: dial directly
			}
		}
		return pu, nil
	}, nil
}

// VerifyAuth probes /api/nutanix/v3/users/me to confirm the configured
// credentials are valid. Used by the login flow as a credential check before
// persisting passwords or starting a session.
func (c *Client) VerifyAuth(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/api/nutanix/v3/users/me", nil, nil)
}

// Reachable probes whether the endpoint host answers, WITHOUT authenticating.
// It sends one short GET (no credentials when the client has no authenticator,
// response body discarded) and treats ANY HTTP response — including 401/403 —
// as reachable; only a transport failure (DNS / TCP / TLS) counts as
// unreachable. Returns the round-trip duration of the attempt.
//
// This backs the cluster-map reachability dot: it must reveal only host up/down
// + latency, never depend on being signed in, and never disclose cluster data.
func (c *Client) Reachable(ctx context.Context) (bool, time.Duration) {
	start := time.Now()
	req, err := c.newRequest(ctx, http.MethodGet, "/api/nutanix/v3/users/me", nil)
	if err != nil {
		return false, 0
	}
	resp, err := c.http.Do(req)
	rtt := time.Since(start)
	if err != nil {
		return false, rtt
	}
	_ = resp.Body.Close()
	return true, rtt
}

func (c *Client) Close() {
	closeIdleConns(c.http.Transport)
}

// Proxy returns the resolved proxy function (REST transport + WS dialer share
// it) so callers dialing outside the http.Client — the VNC WebSocket upgrade —
// route through the same egress proxy / air-gap config (OBS-02). Never nil.
func (c *Client) Proxy() func(*http.Request) (*url.URL, error) {
	return c.proxy
}

// RotateCookies feeds Set-Cookie header values captured OUTSIDE the REST
// transport (the VNC WebSocket upgrade response, NUTA-05) into the session
// cookie jar so memory + keystore stay current. No-op when the client has no
// jar (e.g. the unauthenticated health probe client).
func (c *Client) RotateCookies(setCookie []string) {
	if c.jar != nil {
		c.jar.Rotate(setCookie)
	}
}

// SessionCookies returns a copy of the client's current jar cookies (the
// POST-rotation set after any request), or nil if the client has no jar. The
// caller persists these so memory + keystore carry the rotated session, not the
// pre-rotation captured cookies.
func (c *Client) SessionCookies() []SessionCookie {
	if c.jar == nil {
		return nil
	}
	return c.jar.Snapshot()
}

// closeIdleConns drains idle connections, unwrapping the cookie-capture
// transport (which the jar path installs) to reach the real *http.Transport.
func closeIdleConns(rt http.RoundTripper) {
	switch t := rt.(type) {
	case *http.Transport:
		t.CloseIdleConnections()
	case *cookieCaptureTransport:
		closeIdleConns(t.base)
	}
}

// ----- VMs -----

type VM struct {
	UUID        string
	Name        string
	Description string
	PowerState  string
	ClusterName string
	ClusterUUID string
	PrimaryIP   string
}

type ListVMsOptions struct {
	Search string
	Offset int
	Length int
}

type prismListRequest struct {
	Kind          string `json:"kind"`
	Length        int    `json:"length"`
	Offset        int    `json:"offset"`
	Filter        string `json:"filter,omitempty"`
	SortAttribute string `json:"sort_attribute,omitempty"`
	SortOrder     string `json:"sort_order,omitempty"`
}

type prismVMListResponse struct {
	Entities []struct {
		Metadata struct {
			UUID string `json:"uuid"`
		} `json:"metadata"`
		Status struct {
			Name      string `json:"name"`
			Resources struct {
				PowerState string `json:"power_state"`
				NICList    []struct {
					IPEndpointList []struct {
						IP string `json:"ip"`
					} `json:"ip_endpoint_list"`
				} `json:"nic_list"`
			} `json:"resources"`
			ClusterReference struct {
				Name string `json:"name"`
				UUID string `json:"uuid"`
			} `json:"cluster_reference"`
			Description string `json:"description"`
		} `json:"status"`
	} `json:"entities"`
}

func (c *Client) ListVMs(ctx context.Context, opts ListVMsOptions) ([]VM, error) {
	if opts.Length == 0 {
		opts.Length = 50
	}
	body := prismListRequest{
		Kind:   "vm",
		Length: opts.Length,
		Offset: opts.Offset,
	}
	if opts.Search != "" {
		body.Filter = "vm_name==.*" + opts.Search + ".*"
	}
	var resp prismVMListResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/nutanix/v3/vms/list", body, &resp); err != nil {
		return nil, err
	}
	out := make([]VM, 0, len(resp.Entities))
	for _, e := range resp.Entities {
		ip := ""
		if len(e.Status.Resources.NICList) > 0 && len(e.Status.Resources.NICList[0].IPEndpointList) > 0 {
			ip = e.Status.Resources.NICList[0].IPEndpointList[0].IP
		}
		out = append(out, VM{
			UUID:        e.Metadata.UUID,
			Name:        e.Status.Name,
			Description: e.Status.Description,
			PowerState:  e.Status.Resources.PowerState,
			ClusterName: e.Status.ClusterReference.Name,
			ClusterUUID: e.Status.ClusterReference.UUID,
			PrimaryIP:   ip,
		})
	}
	return out, nil
}

// ----- console websocket -----

// ConsoleWebSocketURL returns the wss:// URL Prism's web UI uses for the
// hypervisor-side VNC stream. Confirmed via HAR capture against pc.7.5.1.4:
//
//	wss://<pc>:9440/vnc/vm/<vm_uuid>/proxy?proxyClusterUuid=<cluster_uuid>
//
// Notes from the working request:
//   - Only `proxyClusterUuid` is in the query string. Adding the older
//     `clusterUuid` parameter causes PC's WS handler to 403 the upgrade.
//   - No `proxyToken` is sent on the URL; auth is carried by the cookie
//     replay alone.
func (c *Client) ConsoleWebSocketURL(vmUUID, clusterUUID string) string {
	host := c.base.Host
	// PathEscape the renderer-supplied VM UUID so a crafted resource ID
	// cannot inject extra path segments / query into the upstream Prism URL.
	esc := url.PathEscape(vmUUID)
	if clusterUUID == "" {
		return fmt.Sprintf("wss://%s/vnc/vm/%s/proxy", host, esc)
	}
	q := url.Values{}
	q.Set("proxyClusterUuid", clusterUUID)
	return fmt.Sprintf("wss://%s/vnc/vm/%s/proxy?%s", host, esc, q.Encode())
}

// ----- v4 console token flow (AHV-04, NEEDS-LIVE-VALIDATION) -----
//
// The v4 path mints a short-lived per-VM VmConsoleToken (a JWT) and dials a
// different WS URL (.../console/launch/<uuid>?VmConsoleToken=<jwt>), still
// carrying the session cookie. It is OPT-IN and used ONLY when the VMM-v4
// capability probe (HasVMMv4) confirms v4 is present; the v3 /vnc/vm/<uuid>/proxy
// dial in ConsoleWebSocketURL remains the DEFAULT/fallback so current behavior
// is preserved.
//
// IMPORTANT: the exact v4 wire shapes below (generate-console-token request +
// task response carrying the JWT, the launch path, and the query param name)
// are implemented to the documented v4 contract but MUST be validated against a
// live Prism Central build before GA. Do not flip the dial default to v4 until
// then. See audit finding AHV-04 (needs-live-validation).

// v4ConsoleTokenResp models the async generate-console-token action's task
// reference (HTTP 202 -> task extId), mirroring v4TaskRef used by power actions.
type v4ConsoleTokenResp = v4TaskRef

// v4ConsoleTokenTaskResp models GET /api/prism/v4.0/config/tasks/{extId} when
// the completed task carries the minted console token. Prism surfaces task
// outputs as a list of typed entities; we scan for the first non-empty token
// field. VALIDATE the exact field name/shape against a live PC before GA.
type v4ConsoleTokenTaskResp struct {
	Data struct {
		Status            string `json:"status"`
		CompletionDetails []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"completionDetails"`
	} `json:"data"`
}

// AcquireConsoleTokenV4 mints a per-VM VmConsoleToken via the v4 VMM
// generate-console-token action, polling the returned task for the JWT. Returns
// the token string. OPT-IN: callers MUST first confirm HasVMMv4. On any failure
// the caller is expected to fall back to the v3 cookie-only dial.
//
// NEEDS-LIVE-VALIDATION (AHV-04): request path, ETag/If-Match requirement, and
// the task-output field carrying the JWT are per the documented v4 contract and
// must be confirmed against a live Prism Central before this path goes GA.
func (c *Client) AcquireConsoleTokenV4(ctx context.Context, vmUUID string) (string, error) {
	vmPath := "/api/vmm/v4.0/ahv/config/vms/" + url.PathEscape(vmUUID)
	// v4 mutating actions are guarded by an If-Match precondition carrying the
	// entity ETag from a prior GET (same contract as power actions).
	etag, err := c.getETag(ctx, vmPath)
	if err != nil {
		return "", fmt.Errorf("get vm etag for console token: %w", err)
	}
	var ref v4ConsoleTokenResp
	status, err := c.doV4Mutation(ctx, vmPath+"/$actions/generate-console-token", etag, nil, &ref)
	if err != nil {
		return "", fmt.Errorf("generate console token: %w", err)
	}
	// Some builds may return the token synchronously (200) in a body; others
	// async (202) via a task. We modeled the async task path; require a task
	// reference on 202 (same guard as NUTA-04).
	if status == http.StatusAccepted && ref.Data.ExtID == "" {
		return "", errors.New("v4 generate-console-token returned no task reference")
	}
	if ref.Data.ExtID == "" {
		return "", errors.New("v4 generate-console-token returned no console token task")
	}
	return c.pollConsoleTokenV4(ctx, ref.Data.ExtID, 2*time.Minute)
}

// pollConsoleTokenV4 polls the generate-console-token task to terminal status
// and extracts the minted JWT from its completion details.
func (c *Client) pollConsoleTokenV4(ctx context.Context, taskExtID string, total time.Duration) (string, error) {
	path := "/api/prism/v4.0/config/tasks/" + url.PathEscape(taskExtID)
	deadline := time.Now().Add(total)
	delay := 200 * time.Millisecond
	for {
		var resp v4ConsoleTokenTaskResp
		if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return "", fmt.Errorf("poll console token task %s: %w", taskExtID, err)
		}
		switch strings.ToUpper(resp.Data.Status) {
		case "SUCCEEDED":
			for _, d := range resp.Data.CompletionDetails {
				if d.Value != "" {
					return d.Value, nil
				}
			}
			return "", errors.New("console token task succeeded but carried no token")
		case "FAILED", "ABORTED", "CANCELED", "CANCELLED":
			return "", fmt.Errorf("console token task %s ended %s", taskExtID, resp.Data.Status)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("console token task %s did not complete within %s (last status %q)", taskExtID, total, resp.Data.Status)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
}

// ConsoleWebSocketURLV4 builds the v4 console launch WS URL:
//
//	wss://<pc>/console/launch/<vm_uuid>?VmConsoleToken=<jwt>
//
// The session cookie is still sent on the upgrade (the token authorizes the
// specific console; the cookie authenticates the session). OPT-IN — only used
// when HasVMMv4 is confirmed; v3 ConsoleWebSocketURL stays the default.
//
// NEEDS-LIVE-VALIDATION (AHV-04): the launch path and the VmConsoleToken query
// param name must be confirmed against a live Prism Central before GA.
func (c *Client) ConsoleWebSocketURLV4(vmUUID, token string) string {
	esc := url.PathEscape(vmUUID)
	q := url.Values{}
	q.Set("VmConsoleToken", token)
	return fmt.Sprintf("wss://%s/console/launch/%s?%s", c.base.Host, esc, q.Encode())
}

// UsersMe fetches the authenticated user's roles + permissions list so we
// can confirm whether they actually have console_access privileges. Many
// "403 The user does not have permission" failures come from RBAC, not
// from AHV Remote Console.
type SelfInfo struct {
	Username    string
	DisplayName string
	Roles       []string
}

func (c *Client) UsersMe(ctx context.Context) (*SelfInfo, error) {
	var resp struct {
		Status struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			Resources   struct {
				AccessControlPolicyReferenceList []struct {
					Name string `json:"name"`
				} `json:"access_control_policy_reference_list"`
			} `json:"resources"`
		} `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/nutanix/v3/users/me", nil, &resp); err != nil {
		return nil, err
	}
	out := &SelfInfo{
		Username:    resp.Status.Name,
		DisplayName: resp.Status.DisplayName,
	}
	for _, r := range resp.Status.Resources.AccessControlPolicyReferenceList {
		out.Roles = append(out.Roles, r.Name)
	}
	return out, nil
}

// GetVM fetches a single VM's metadata so we can read its cluster_reference
// UUID (needed for the PC-style console WS URL) without hauling the whole
// list-VMs payload every time the operator opens a console.
type VMDetail struct {
	UUID        string
	Name        string
	ClusterUUID string
	PowerState  string // "ON" / "OFF" from status.resources at read time
}

func (c *Client) GetVM(ctx context.Context, vmUUID string) (*VMDetail, error) {
	var resp struct {
		Metadata struct {
			UUID string `json:"uuid"`
		} `json:"metadata"`
		Status struct {
			Name             string `json:"name"`
			ClusterReference struct {
				UUID string `json:"uuid"`
			} `json:"cluster_reference"`
			Resources struct {
				PowerState string `json:"power_state"`
			} `json:"resources"`
		} `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), nil, &resp); err != nil {
		return nil, err
	}
	return &VMDetail{
		UUID:        resp.Metadata.UUID,
		Name:        resp.Status.Name,
		ClusterUUID: resp.Status.ClusterReference.UUID,
		PowerState:  resp.Status.Resources.PowerState,
	}, nil
}

// SerialPortResult describes what EnsureSerialPort did.
type SerialPortResult struct {
	AlreadyPresent bool   // a port at the requested index was already in the spec
	Added          bool   // the serial port edit task SUCCEEDED (port is in the spec)
	PowerState     string // "ON", "OFF", etc. from status at read time
	// PowerCycleRequired is true when the VM was powered ON. AHV cannot
	// hot-add a serial port, so the new COM port only materializes after a
	// full power OFF→ON. NOTE: an in-guest ACPI *reboot* is NOT enough — it
	// does not restart the QEMU process, so the device never appears. The
	// operator must fully power the VM off and back on.
	PowerCycleRequired bool
}

// EnsureSerialPort ensures the VM has a serial port at the given index
// (index 2 => COM3 in Windows) for the vdagent clipboard channel, adding one
// if missing. Idempotent — no-op if already present.
//
// Two AHV semantics make this trickier than it looks (and both were bugs in
// the original fire-and-forget version, where a rejected edit silently looked
// like success and COM3 never appeared — the reported sysvpn10 failure):
//
//   - The Prism v3 PUT /vms is ASYNCHRONOUS: a 2xx only means the edit was
//     ACCEPTED. We MUST poll the returned task to learn whether it actually
//     applied, exactly like AttachCDROM does.
//   - AHV cannot HOT-ADD a serial port to a running VM. When the VM is ON the
//     edit task is rejected (or, on builds that persist the spec, the device
//     still needs a power cycle). Either way the operator must power the VM
//     OFF and back ON — reported via PowerCycleRequired so the UI gives the
//     correct instruction instead of the old, misleading "needs reboot".
func (c *Client) EnsureSerialPort(ctx context.Context, vmUUID string, index int) (*SerialPortResult, error) {
	res, err := c.ensureSerialPortOnce(ctx, vmUUID, index)
	if isConflict409(err) {
		// One-shot 409 retry, same as the other VM spec-update paths: a fresh
		// GET picks up the new spec_version, re-apply the mutation, PUT once.
		if c.log != nil {
			c.log.Warn("serial port add hit 409 — refetching spec and retrying once", "vm", vmUUID)
		}
		res, err = c.ensureSerialPortOnce(ctx, vmUUID, index)
	}
	return res, err
}

// ensureSerialPortOnce is a single GET → mutate → PUT → task-wait attempt;
// EnsureSerialPort wraps it with the one-shot 409 retry.
func (c *Client) ensureSerialPortOnce(ctx context.Context, vmUUID string, index int) (*SerialPortResult, error) {
	var full json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), nil, &full); err != nil {
		return nil, fmt.Errorf("get vm: %w", err)
	}

	var parsed struct {
		Spec struct {
			Resources struct {
				SerialPortList []struct {
					Index int `json:"index"`
				} `json:"serial_port_list"`
			} `json:"resources"`
		} `json:"spec"`
		Status struct {
			Resources struct {
				PowerState string `json:"power_state"`
			} `json:"resources"`
		} `json:"status"`
	}
	if err := json.Unmarshal(full, &parsed); err != nil {
		return nil, fmt.Errorf("parse vm spec: %w", err)
	}

	powerState := parsed.Status.Resources.PowerState // "ON", "OFF"
	poweredOn := strings.EqualFold(powerState, "ON")

	for _, sp := range parsed.Spec.Resources.SerialPortList {
		if sp.Index == index {
			return &SerialPortResult{AlreadyPresent: true, PowerState: powerState}, nil
		}
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(full, &doc); err != nil {
		return nil, fmt.Errorf("parse vm doc: %w", err)
	}

	spec, _ := doc["spec"].(map[string]interface{})
	if spec == nil {
		return nil, fmt.Errorf("vm spec is nil")
	}
	resources, _ := spec["resources"].(map[string]interface{})
	if resources == nil {
		return nil, fmt.Errorf("vm resources is nil")
	}

	portList, _ := resources["serial_port_list"].([]interface{})
	portList = append(portList, map[string]interface{}{
		"index":        index,
		"is_connected": true,
	})
	resources["serial_port_list"] = portList

	// PUT the whole document back minus status (Prism rejects status on
	// write). metadata — including spec_version — is preserved by round-
	// tripping doc, which is what avoids a 409 edit conflict.
	delete(doc, "status")

	var putResp prismCreateImageResp // /vms PUT shares this shape: metadata.uuid + execution_context.task_uuid
	if err := c.doJSON(ctx, http.MethodPut, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), doc, &putResp); err != nil {
		return nil, fmt.Errorf("add serial port: %w", err)
	}

	// Poll the edit task — the 2xx above only means "accepted".
	taskUUID := string(putResp.Status.ExecutionContext.TaskUUID)
	if err := c.waitForTask(ctx, taskUUID, 2*time.Minute); err != nil {
		// On a running VM the failure is almost always "cannot hot-add a
		// serial port". Actionable guidance (power-cycle) helps the operator
		// far more than the raw task error, so we surface that state instead
		// of a hard error and keep the detail in the log for diagnostics.
		if poweredOn {
			if c.log != nil {
				c.log.Warn("serial port add task failed on powered-on VM (expected — needs power cycle)",
					"vm", vmUUID, "err", err)
			}
			return &SerialPortResult{PowerState: powerState, PowerCycleRequired: true}, nil
		}
		return nil, fmt.Errorf("serial port add task failed: %w", err)
	}

	// Task succeeded. If the VM was ON, Prism accepted the spec edit but the
	// device still needs a full power cycle before the guest enumerates it.
	return &SerialPortResult{Added: true, PowerState: powerState, PowerCycleRequired: poweredOn}, nil
}

// powerVerbs maps each operator power action to its Prism Central v4 VMM
// action subresource and whether that action carries a guest power-state
// transition body. The v3 intent API (PUT vms/{uuid} with power_state) accepts
// only ON/OFF — reboot, reset and power-cycle have NO v3 representation — so
// power actions go through the v4 dedicated $actions subresources instead.
// (See audit finding AHV-01: the old code PUT an invalid power_state="RESET".)
//
//	ON            -> power-on       (hard, no body)
//	OFF           -> power-off      (hard "force off", no body)
//	ACPI_SHUTDOWN -> guest-shutdown (graceful; NGT when present, ACPI otherwise)
//	ACPI_REBOOT   -> guest-reboot   (graceful restart)
//	RESET         -> reset          (hard reset, no body)
var powerVerbs = map[string]struct {
	verb  string
	guest bool
}{
	"ON":            {"power-on", false},
	"OFF":           {"power-off", false},
	"ACPI_SHUTDOWN": {"guest-shutdown", true},
	"ACPI_REBOOT":   {"guest-reboot", true},
	"RESET":         {"reset", false},
}

// guestPowerStateTransitionConfig is the v4 body for guest-shutdown and
// guest-reboot. We disable in-guest (NGT) pre-transition script execution so
// the action is a plain graceful shutdown/reboot — matching the operator-facing
// "ACPI" semantics — rather than running guest scripts. Fields are camelCase
// per the v4 wire convention.
type guestPowerStateTransitionConfig struct {
	ShouldEnableScriptExec    bool `json:"shouldEnableScriptExec"`
	ShouldFailOnScriptFailure bool `json:"shouldFailOnScriptFailure"`
}

// v4TaskRef is the TaskReference an async v4 action returns (HTTP 202).
type v4TaskRef struct {
	Data struct {
		ExtID string `json:"extId"`
	} `json:"data"`
}

// v4TaskResp models GET /api/prism/v4.0/config/tasks/{extId}. Beyond the
// status, a terminal failure carries its reason in errorMessages (v4
// AppMessage list) or, on some builds, legacyError — both parsed so the
// operator sees WHY a power action failed, not just that it did.
type v4TaskResp struct {
	Data struct {
		Status        string `json:"status"`
		ErrorMessages []struct {
			Message string `json:"message"`
		} `json:"errorMessages"`
		LegacyError struct {
			Message string `json:"message"`
		} `json:"legacyError"`
	} `json:"data"`
}

// errorDetail flattens a terminal task's failure payload into one line.
func (r *v4TaskResp) errorDetail() string {
	var parts []string
	for _, m := range r.Data.ErrorMessages {
		if t := strings.TrimSpace(m.Message); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		if t := strings.TrimSpace(r.Data.LegacyError.Message); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return "Prism reported no failure detail — check the task in the Prism UI"
	}
	return strings.Join(parts, "; ")
}

// taskDisplayID strips the ergon composite prefix ("ZXJnb24=:<uuid>") from a
// v4 task extId for operator-facing messages — the base64 service tag is wire
// plumbing, not information.
func taskDisplayID(extID string) string {
	if i := strings.LastIndexByte(extID, ':'); i >= 0 && i+1 < len(extID) {
		return extID[i+1:]
	}
	return extID
}

// vmmV4Root is the VMM v4 namespace root we probe for capability detection
// (NUTA-01). A reachable, authenticated PC that exposes VMM v4 answers any
// status here EXCEPT 404 (namespace absent). A v3-only PC dead-ends here.
const vmmV4Root = "/api/vmm/v4.0/ahv/config/vms"

// HasVMMv4 reports whether this PC exposes the VMM v4 namespace power actions
// rely on, probing exactly once per Client (per endpoint) and caching the
// result (NUTA-01). The probe is a cheap GET on the v4 VMs collection root: a
// 404 means the namespace is absent (a v3-only PC), anything else (200, 401,
// 403, 400, 5xx) means the namespace IS routed and v4 is available — we treat
// "present but unauthorized/erroring" as available because that is a session or
// transient problem, not a missing-capability problem. A transport failure
// (cannot reach PC at all) is treated as NOT available so the caller surfaces a
// reachability error rather than a v4-specific one; the next call re-probes.
func (c *Client) HasVMMv4(ctx context.Context) bool {
	c.v4mu.Lock()
	defer c.v4mu.Unlock()
	if c.v4Probed {
		return c.v4Available
	}
	req, err := c.newRequest(ctx, http.MethodGet, vmmV4Root, nil)
	if err != nil {
		// Couldn't even build the probe request — treat as not available but do
		// NOT cache, so a later call retries.
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Reachability failure — don't cache a misleading "no v4"; the next call
		// (when the PC is reachable) re-probes.
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	// The probe reached the PC: cache the answer. 404 == namespace not routed ==
	// v3-only PC; anything else (200/401/403/4xx/5xx) == namespace IS routed.
	c.v4Available = resp.StatusCode != http.StatusNotFound
	c.v4Probed = true
	return c.v4Available
}

// PowerAction performs a VM power action through the Prism Central v4 VMM
// action subresources (extId == VM UUID). It GETs the VM to obtain the entity
// ETag (v4 mutations require it as the If-Match precondition), POSTs the mapped
// $actions/<verb> with a fresh Ntnx-Request-Id idempotency key, then polls the
// returned task to a terminal state so the caller learns real success/failure
// instead of mere acceptance.
//
// action is one of: ON, OFF, ACPI_SHUTDOWN, ACPI_REBOOT, RESET.
//
// NOTE: implemented to the documented v4 contract (ETag/If-Match,
// Ntnx-Request-Id idempotency, $actions verbs, GuestPowerStateTransitionConfig
// camelCase body, /prism/v4.0/config/tasks task poll). The exact wire shapes
// should be validated against a live Prism Central build before GA.
func (c *Client) PowerAction(ctx context.Context, vmUUID, action string) error {
	pv, ok := powerVerbs[action]
	if !ok {
		return fmt.Errorf("unsupported power action %q", action)
	}
	// NUTA-01: a non-VMM-v4 PC dead-ends every power button on a raw getETag
	// 404. Probe (once, cached) whether the VMM v4 namespace is even present and
	// return an ACTIONABLE error instead of a bare HTTP 404 the operator can't
	// interpret.
	if !c.HasVMMv4(ctx) {
		return fmt.Errorf("power actions require a Prism Central with VMM v4; this PC reports v3 (action %q unavailable)", action)
	}
	vmPath := "/api/vmm/v4.0/ahv/config/vms/" + url.PathEscape(vmUUID)

	// v4 mutations are guarded by an If-Match precondition carrying the
	// entity ETag from a prior GET.
	etag, err := c.getETag(ctx, vmPath)
	if err != nil {
		// Defense in depth (NUTA-01): even with the capability probe above, a
		// 404 here means the v4 VM entity/namespace isn't serving this path on
		// this PC. Classify it into the same actionable message rather than
		// surfacing a raw "HTTP 404" the operator can't act on.
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return fmt.Errorf("power actions require a Prism Central with VMM v4; this PC reports v3 (action %q unavailable)", action)
		}
		return fmt.Errorf("get vm etag for power %s: %w", action, err)
	}

	var body any
	if pv.guest {
		body = guestPowerStateTransitionConfig{}
	}
	var ref v4TaskRef
	status, err := c.doV4Mutation(ctx, vmPath+"/$actions/"+pv.verb, etag, body, &ref)
	if err != nil {
		return fmt.Errorf("power %s: %w", action, err)
	}

	// NUTA-04: a 202 Accepted means the action is ASYNC and MUST hand back a
	// task extId to poll. An empty extId on a 202 is not success — it means we
	// have no task reference to confirm the action ever ran, so surface it as an
	// error instead of silently reporting success. The synchronous-success
	// short-circuit (no task to poll) is allowed ONLY for 200/204.
	if status == http.StatusAccepted && ref.Data.ExtID == "" {
		return fmt.Errorf("power %s: v4 power action returned no task reference", action)
	}
	return c.waitForTaskV4(ctx, ref.Data.ExtID, 2*time.Minute)
}

// getETag issues a GET and returns the response ETag header — the precondition
// value v4 mutations echo via If-Match. The body is drained and discarded.
func (c *Client) getETag(ctx context.Context, path string) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", &APIError{Method: http.MethodGet, Path: path, Status: resp.StatusCode}
	}
	return resp.Header.Get("ETag"), nil
}

// doV4Mutation issues a v4 mutating POST with the If-Match precondition (when
// non-empty) and a fresh Ntnx-Request-Id idempotency key, decoding the 2xx body
// into out. It returns the HTTP status code so the caller can distinguish an
// async 202 (which MUST carry a task extId to poll) from a synchronous 200/204
// success (NUTA-04). On 204 (no content) the body decode is skipped.
func (c *Client) doV4Mutation(ctx context.Context, path, ifMatch string, body, out any) (int, error) {
	req, err := c.newRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return 0, err
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	req.Header.Set("Ntnx-Request-Id", uuid.NewString())
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, &APIError{Method: http.MethodPost, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		// A 200/204 sync success may legitimately carry no JSON body; tolerate
		// an empty body rather than failing on EOF.
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// waitForTaskV4 polls /api/prism/v4.0/config/tasks/{extId} until the task
// reaches a terminal status or total elapses. Empty extId short-circuits as
// success (the action returned synchronously).
func (c *Client) waitForTaskV4(ctx context.Context, taskExtID string, total time.Duration) error {
	if taskExtID == "" {
		return nil
	}
	deadline := time.Now().Add(total)
	delay := 200 * time.Millisecond
	for {
		snap, err := c.PollTaskOnce(ctx, taskExtID, true)
		if err != nil {
			return fmt.Errorf("poll power task %s: %w", taskDisplayID(taskExtID), err)
		}
		switch snap.State {
		case "succeeded":
			return nil
		case "failed":
			return fmt.Errorf("power task %s ended FAILED: %s",
				taskDisplayID(taskExtID), snap.Error)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("power task %s did not complete within %s",
				taskDisplayID(taskExtID), total)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
}

// WSDialHeader constructs the upgrade headers Prism's web UI actually sends
// for the VNC WebSocket, confirmed via HAR against pc.7.5.1.4:
//
//	Origin: https://<pc>
//	Cookie: <session cookies>
//	User-Agent: <Chrome>
//	Accept-Language: en-US,en;q=0.9
//	Cache-Control: no-cache
//	Pragma: no-cache
//
// The browser sends NO Sec-Fetch-*, NO Referer, NO X-XSRF-TOKEN,
// NO X-NTNX-CSRF-Token, and NO Sec-WebSocket-Protocol on this request.
// Earlier revisions added all of those; PC's IGW rejects the upgrade
// when extras are present (the rest of the auth chain treats anything
// that doesn't look exactly like the in-page console as an outside
// attempt). Mirroring the browser's wire format exactly is what makes
// it accept the upgrade.
func (c *Client) WSDialHeader(ctx context.Context) (http.Header, error) {
	out := http.Header{}
	if c.auth == nil {
		return out, nil
	}
	dummy, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String(), nil)
	if err != nil {
		return nil, err
	}
	if err := c.auth.Apply(ctx, dummy); err != nil {
		return nil, err
	}
	if v := dummy.Header.Get("Authorization"); v != "" {
		out.Set("Authorization", v)
	}
	if v := dummy.Header.Get("Cookie"); v != "" {
		out.Set("Cookie", v)
	}
	out.Set("Origin", fmt.Sprintf("https://%s", c.base.Host))
	out.Set("User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "+
			"(KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36")
	out.Set("Accept-Language", "en-US,en;q=0.9")
	out.Set("Cache-Control", "no-cache")
	out.Set("Pragma", "no-cache")
	return out, nil
}

// ----- low-level helpers -----

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	u, err := c.base.Parse(path)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AHV Remote Console/AHV")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.auth != nil {
		if err := c.auth.Apply(ctx, req); err != nil {
			return nil, fmt.Errorf("authenticator: %w", err)
		}
	}
	return req, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 32*1024*1024)).Decode(out)
	}
	return nil
}

// APIError is a structured failure response from the upstream so callers
// can branch on the HTTP status without parsing error strings. doJSON and
// VerifyAuth return this for any non-2xx response.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

// Error deliberately OMITS Body: an upstream WAF can reflect the request's
// Cookie/Authorization headers into a 4xx/5xx response body, and Error()'s
// string flows into operator-facing toasts and the on-disk audit log. Callers
// that need the body for branching read e.Body directly (see images_gc.go); it
// must never reach a log/UI sink. Keep Method/Path/Status — those are safe.
func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// IsUnauthorized reports whether err signals an authentication failure
// (HTTP 401 or 403). Used by the plugin to distinguish "creds expired,
// kick the operator back to sign-in" from generic upstream errors.
func IsUnauthorized(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 401 || ae.Status == 403
	}
	return false
}

// HTTPStatus returns the upstream HTTP status carried by an *APIError, or 0 if
// err is not an APIError. Used by OpenStream's power-state preflight to map a
// v3 VM read into a stable, classifiable dial cause WITHOUT surfacing the
// (credential-reflecting) response body.
func HTTPStatus(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}
