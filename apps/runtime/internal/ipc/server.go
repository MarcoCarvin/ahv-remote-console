// Package ipc implements the local loopback HTTP+WebSocket server used by the
// Electron renderer to talk to the runtime. Every request is authenticated
// with an HMAC-SHA256 signature over the request line and body. The server is
// bound to 127.0.0.1 only and rejects any Host header that is not loopback.
package ipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/audit"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/config"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/keystore"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/netstate"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/presence"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/telemetry"
)

type Options struct {
	Secret    *Secret
	Logger    *slog.Logger
	Telemetry *telemetry.Tracer
	Plugins   *plugins.Host
	NetState  *netstate.Observer
	Keystore  keystore.Store
	Config    *config.Manager
	// Audit is the append-only event recorder. Required (use audit.Open
	// with enabled=false to get a no-op logger if audit is disabled by
	// config).
	Audit    *audit.Logger
	Presence *presence.Hub
}

type Server struct {
	opts   Options
	http   *http.Server
	mux    *http.ServeMux
	router *router
}

func NewServer(opts Options) (*Server, error) {
	if opts.Secret == nil {
		return nil, errors.New("ipc: nil secret")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	mux := http.NewServeMux()
	router := newRouter(opts)
	router.attach(mux)

	httpSrv := &http.Server{
		Handler:           withMiddleware(mux, opts),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // streaming endpoints handle their own timeouts
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slogToStd(opts.Logger),
		// ConnContext logs the connecting peer's PID where the OS
		// supports it (Linux SO_PEERCRED on TCP loopback). On macOS/
		// Windows the PID isn't exposed for TCP loopback so we record
		// (0, "unknown") and document the gap as v2 work.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			pid, ok := peerPID(c)
			if !ok {
				opts.Logger.Warn("ipc: peer pid lookup failed",
					"remote", c.RemoteAddr().String())
				return ctx
			}
			if pid != 0 {
				opts.Logger.Debug("ipc: accepted connection",
					"remote", c.RemoteAddr().String(),
					"peer_pid", pid)
			}
			return ctx
		},
	}
	return &Server{opts: opts, http: httpSrv, mux: mux, router: router}, nil
}

func (s *Server) Serve(l net.Listener) error {
	if err := s.http.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// ---------- middleware ----------

func withMiddleware(next http.Handler, opts Options) http.Handler {
	return loopbackOnly(opts.Logger,
		corsBridge(
			requestID(
				recoverer(opts.Logger,
					hmacAuth(opts.Secret, opts.Logger, next)))))
}

// corsBridge handles the cross-origin handshake between the renderer and the
// runtime. The renderer's origin is either "null" (Electron production, page
// loaded from file://) or http://localhost:5173 (Vite dev server). For any
// other origin we omit the headers entirely, which makes the browser block
// the request. OPTIONS preflights short-circuit here so they never reach the
// HMAC layer, which would reject them because preflights cannot carry our
// custom signature headers.
func corsBridge(next http.Handler) http.Handler {
	// Production Electron (28+, we ship 42) loads the renderer from file:// and
	// emits the literal "file://" Origin; the Vite dev server emits
	// http://localhost:5173. We do NOT accept the opaque "null" Origin: pairing
	// "null" with Access-Control-Allow-Credentials would let any context the
	// browser collapses to "null" (sandboxed iframe, data:) reach the loopback
	// runtime WITH credentials. The upgrade is also HMAC-signed, but the CORS
	// allowlist must not widen the surface (audit SEC-03).
	allowed := map[string]struct{}{
		"file://":               {},
		"http://localhost:5173": {},
		"http://127.0.0.1:5173": {},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set(
					"Access-Control-Allow-Headers",
					"Content-Type, Accept, X-Request-Id, X-HC-Sig, X-HC-Timestamp, X-HC-Nonce",
				)
				w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackOnly(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			log.Warn("rejecting non-loopback connection", "remote", r.RemoteAddr)
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		// Block DNS rebinding via Host header.
		hostHeader, _, _ := net.SplitHostPort(r.Host)
		if hostHeader == "" {
			hostHeader = r.Host
		}
		if hostHeader != "127.0.0.1" && hostHeader != "localhost" && hostHeader != "[::1]" {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxRequestID{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic in handler",
					"path", r.URL.Path,
					"recover", fmt.Sprintf("%v", rec))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// hmacAuth verifies the HMAC over the canonical request: METHOD + "\n" + PATH
// + "\n" + TIMESTAMP + "\n" + NONCE + "\n" + BODY_SHA256_HEX.
// The signature lives in X-HC-Sig (hex). Timestamp must be within ±60s of now.
// Nonces are tracked in a sliding window to prevent replay.
//
// WebSocket upgrade requests pass the signature in the
// `Sec-WebSocket-Protocol` header as `hc.v1.<sig>.<ts>.<nonce>` because some
// proxies strip custom headers from upgrade requests. The renderer must echo
// back the negotiated subprotocol.
func hmacAuth(secret *Secret, log *slog.Logger, next http.Handler) http.Handler {
	const skew = 60 * time.Second
	nonces := newNonceCache(8192, 2*skew)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness/readiness probes and the metrics scrape are HMAC-exempt
		// (OBS-01): they expose only coarse health/gauges (no secrets, no
		// per-cluster detail) and an orchestrator/Prometheus scraper can't carry
		// the IPC secret. They remain loopback-only via loopbackOnly.
		switch r.URL.Path {
		case "/healthz", "/readyz", "/metrics", "/version":
			next.ServeHTTP(w, r)
			return
		}

		ts, nonce, sigHex, ok := extractAuthMaterial(r)
		if !ok {
			http.Error(w, "missing signature", http.StatusUnauthorized)
			return
		}
		t, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			http.Error(w, "invalid timestamp", http.StatusUnauthorized)
			return
		}
		when := time.Unix(t, 0)
		if d := time.Since(when); d > skew || d < -skew {
			http.Error(w, "stale or future-dated request", http.StatusUnauthorized)
			return
		}
		if !nonces.use(nonce, when) {
			http.Error(w, "replay detected", http.StatusUnauthorized)
			return
		}

		// Cap the body BEFORE buffering it whole into memory to compute the
		// HMAC. Without this a (secret-holding) caller could stream an
		// unbounded body and exhaust memory. 8 MiB comfortably covers every
		// legitimate IPC payload (config/auth/clipboard-stage requests).
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		_ = r.Body.Close()
		bodyHash := sha256.Sum256(body)
		canonical := []byte(strings.Join([]string{
			r.Method, r.URL.RequestURI(), ts, nonce, hex.EncodeToString(bodyHash[:]),
		}, "\n"))
		expected := secret.MAC(canonical)
		got, err := hex.DecodeString(sigHex)
		if err != nil || !ConstantTimeEqual(expected, got) {
			log.Warn("HMAC mismatch", "path", r.URL.Path)
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		// Restore body for downstream handlers.
		r.Body = io.NopCloser(bytesReader(body))
		next.ServeHTTP(w, r)
	})
}

func extractAuthMaterial(r *http.Request) (ts, nonce, sig string, ok bool) {
	// Preferred: dedicated headers.
	sig = r.Header.Get("X-HC-Sig")
	ts = r.Header.Get("X-HC-Timestamp")
	nonce = r.Header.Get("X-HC-Nonce")
	if sig != "" && ts != "" && nonce != "" {
		return ts, nonce, sig, true
	}
	// Fallback for WebSocket: encoded in Sec-WebSocket-Protocol.
	for _, proto := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(proto, ",") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, "hc.v1.") {
				continue
			}
			toks := strings.Split(strings.TrimPrefix(part, "hc.v1."), ".")
			if len(toks) == 3 {
				return toks[1], toks[2], toks[0], true
			}
		}
	}
	return "", "", "", false
}

type ctxRequestID struct{}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": msg,
		},
	})
}

func bytesReader(b []byte) io.Reader {
	return &bufReader{data: b}
}

type bufReader struct {
	data []byte
	off  int
}

func (r *bufReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func slogToStd(l *slog.Logger) *log.Logger {
	return log.New(&slogWriter{log: l}, "", 0)
}

type slogWriter struct{ log *slog.Logger }

func (s *slogWriter) Write(p []byte) (int, error) {
	s.log.Warn("http server", "msg", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
