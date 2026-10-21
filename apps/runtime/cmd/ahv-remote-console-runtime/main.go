// Package main is the entry point for the AHV Remote Console local runtime daemon.
//
// The runtime is a single-user, loopback-only HTTP+WebSocket server that
// brokers all privileged communication between the Electron renderer and
// the upstream Nutanix Prism Central (or other plugin) endpoints. It is
// never reachable from outside 127.0.0.1.
//
// The renderer and the runtime authenticate every IPC call with HMAC-SHA256
// using an ephemeral per-launch secret. The secret is either passed via
// stdin (production) or read from an environment variable (development).
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/audit"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/build"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/config"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/handshake"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/ipc"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/keystore"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/logging"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/netstate"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/presence"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ahv-remote-console-runtime: %v\n", err)
		os.Exit(1)
	}
}

type flags struct {
	logLevel       string
	logFormat      string
	logFile        string
	dataDir        string
	pluginsDir     string
	ipcSecretEnv   string
	ipcSecretStdin bool
	telemetryURL   string
	version        bool
}

func parseFlags() (*flags, error) {
	f := &flags{}
	flag.StringVar(&f.logLevel, "log-level", "info", "log level: debug|info|warn|error")
	flag.StringVar(&f.logFormat, "log-format", "json", "log format: json|text")
	flag.StringVar(&f.logFile, "log-file", "", "log file path (default: stderr)")
	flag.StringVar(&f.dataDir, "data-dir", "", "runtime data directory (default: OS-specific)")
	flag.StringVar(&f.pluginsDir, "plugins-dir", "", "plugins directory (default: <data-dir>/plugins)")
	flag.StringVar(&f.ipcSecretEnv, "ipc-secret-env", "",
		"environment variable name carrying the IPC HMAC secret (dev only)")
	flag.BoolVar(&f.ipcSecretStdin, "ipc-secret-stdin", false,
		"read IPC HMAC secret as first line on stdin (production)")
	flag.StringVar(&f.telemetryURL, "otlp-endpoint", "",
		"OTLP HTTP endpoint for tracing (disabled if empty)")
	flag.BoolVar(&f.version, "version", false, "print version and exit")
	flag.Parse()
	if f.version {
		fmt.Println(build.Info())
		os.Exit(0)
	}
	return f, nil
}

func run() error {
	f, err := parseFlags()
	if err != nil {
		return err
	}

	log, closer, err := logging.New(logging.Options{
		Level:  f.logLevel,
		Format: f.logFormat,
		File:   f.logFile,
	})
	if err != nil {
		return fmt.Errorf("init logging: %w", err)
	}
	defer func() { _ = closer.Close() }()
	log = log.With("version", build.Version, "commit", build.Commit)

	log.Info("ahv-remote-console runtime starting", "platform", currentPlatform())

	// ---- Resolve data dirs ----
	dataDir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	pluginsDir := f.pluginsDir
	if pluginsDir == "" {
		pluginsDir = filepath.Join(dataDir, "plugins")
	}
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return fmt.Errorf("create plugins dir: %w", err)
	}
	log.Info("data directories ready", "data_dir", dataDir, "plugins_dir", pluginsDir)

	// ---- Resolve IPC secret ----
	secret, err := loadIPCSecret(f)
	if err != nil {
		return fmt.Errorf("load IPC secret: %w", err)
	}
	defer secret.Wipe()
	if secret.Len() < 32 {
		return errors.New("IPC secret must be at least 32 bytes (256 bits)")
	}

	// ---- Config ----
	// Load config BEFORE telemetry so config.Telemetry (Enabled / OTLPEndpoint /
	// SampleRate) can drive tracer construction (OBS-01). The --otlp-endpoint
	// flag remains an override for the configured endpoint so ops can point a
	// build at a collector without editing config.json.
	cfg, err := config.Load(filepath.Join(dataDir, "config.json"), log)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// ---- Init telemetry ----
	telCfg := cfg.Get().Telemetry
	otlpEndpoint := telCfg.OTLPEndpoint
	if f.telemetryURL != "" {
		otlpEndpoint = f.telemetryURL // flag overrides config
	}
	// The --otlp-endpoint flag is a convenience override: passing it implies the
	// operator wants tracing on for this run even if config has it disabled.
	telEnabled := telCfg.Enabled || f.telemetryURL != ""
	tel, err := telemetry.New(telemetry.Options{
		ServiceName:    "ahv-remote-console-runtime",
		ServiceVersion: build.Version,
		Enabled:        telEnabled,
		OTLPEndpoint:   otlpEndpoint,
		SampleRate:     telCfg.SampleRate,
		Logger:         log,
	})
	if err != nil {
		log.Warn("telemetry init failed, continuing without traces", "err", err)
		tel = telemetry.Noop()
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(shutCtx)
	}()

	// ---- Secure keystore ----
	ks, err := keystore.Open(keystore.Options{
		ServiceName: "ahvRemoteConsole",
		Logger:      log,
	})
	if err != nil {
		return fmt.Errorf("open keystore: %w", err)
	}

	// ---- Network state observer ----
	// Root the 2s poll in a cancellable context so it stops on graceful
	// shutdown instead of polling past it (GO-01/GO-R-05). Cancelled in the
	// shutdown block below.
	ns := netstate.New(log)
	nsCtx, nsCancel := context.WithCancel(context.Background())
	defer nsCancel()
	go ns.Run(nsCtx)

	// ---- Plugin host ----
	// Pass the runtime-owned log paths so the diagnostics bundle can tail-include
	// them (DX-S-03). RuntimeLogPath is empty when logs go to stderr (no file to
	// tail); the audit path is always passed (the bundle skips it if the file
	// doesn't exist because audit is disabled).
	host, err := plugins.NewHost(plugins.Options{
		Dir:            pluginsDir,
		Logger:         log,
		Keystore:       ks,
		Telemetry:      tel,
		NetState:       ns,
		Config:         cfg,
		AuditLogPath:   filepath.Join(dataDir, "audit.log"),
		RuntimeLogPath: f.logFile,
	})
	if err != nil {
		return fmt.Errorf("init plugin host: %w", err)
	}
	// NOTE: host.LoadAll() is deliberately deferred until AFTER the IPC
	// listener is bound and the handshake file is written (see below). A
	// plugin's Init() may perform bounded network I/O (each plugin gets a
	// multi-second budget), and running it here previously delayed the
	// handshake file that the Electron shell waits on — making a
	// healthy-but-slow cold start look like a hung/broken launch. We bind +
	// write the handshake first (so the shell flips to "ready" promptly),
	// then load plugins before we begin serving requests.
	var pluginLoadErr error

	// ---- Audit log ----
	cfgSnap := cfg.Get()
	auditLog, err := audit.Open(
		filepath.Join(dataDir, "audit.log"),
		cfgSnap.Security.AuditTrailEnabled,
		log,
	)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer func() { _ = auditLog.Close() }()
	auditLog.Append(audit.ActionRuntimeStart, audit.Record{
		Extra: map[string]any{
			"version": build.Version,
			"commit":  build.Commit,
		},
	})
	defer auditLog.Append(audit.ActionRuntimeStop, audit.Record{})

	// The runtime is a loopback VNC-proxy + plugin host: it terminates
	// the browser/Electron VNC session locally and fans console traffic
	// out to the AHV plugin's TLS-pinned client. Clipboard is not routed
	// through Prism or NGT; it is bridged by the in-guest vdagent over an
	// ISA serial chardev, with the agent delivered as a CD-ROM/ISO install
	// (a one-time per-template task). No per-session listener and no
	// command-routing into the guest from the runtime side.

	// ---- Presence hub ----
	presenceHub := presence.NewHub(log)
	go presenceHub.Run()
	defer presenceHub.Stop()

	// ---- IPC server ----
	srv, err := ipc.NewServer(ipc.Options{
		Secret:    secret,
		Logger:    log,
		Telemetry: tel,
		Plugins:   host,
		NetState:  ns,
		Keystore:  ks,
		Config:    cfg,
		Audit:     auditLog,
		Presence:  presenceHub,
	})
	if err != nil {
		return fmt.Errorf("init IPC server: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind loopback: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	// Write handshake file so the Electron shell can discover the
	// runtime. The handshake carries the loopback IPC endpoint + a
	// secret-hash for the shell's mutual-verification step; bridge
	// distribution lives entirely on the AHV-plugin / Image-Service
	// path now (see /v1/agent/mount-iso) so no out-of-band URL
	// needs to be advertised here.
	hs := handshake.Handshake{
		Version:    build.Version,
		PID:        os.Getpid(),
		Endpoint:   fmt.Sprintf("http://127.0.0.1:%d", port),
		SecretHash: secret.HashB64(),
		StartedAt:  time.Now().UTC(),
	}
	if err := handshake.Write(filepath.Join(dataDir, "runtime.json"), hs); err != nil {
		return fmt.Errorf("write handshake: %w", err)
	}
	defer handshake.Remove(filepath.Join(dataDir, "runtime.json"))

	log.Info("IPC bound", "endpoint", hs.Endpoint, "data_dir", dataDir)

	// ---- Load plugins (deferred from earlier; see note above) ----
	// The handshake is already on disk, so the Electron shell has flipped
	// to "ready" and rendered its UI. Plugins load here, before Serve()
	// begins accepting requests: any renderer call that arrives early
	// simply waits in the listener backlog until Serve starts, rather
	// than blocking the handshake the shell was waiting on.
	pluginLoadErr = host.LoadAll()
	if pluginLoadErr != nil {
		log.Warn("some plugins failed to load", "err", pluginLoadErr)
		auditLog.Append(audit.ActionPluginLoadFailure, audit.Record{
			Result: "error", Reason: pluginLoadErr.Error(),
		})
	}

	// ---- Shutdown signals ----
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Run server.
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(listener) }()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, io.EOF) {
			log.Error("server error", "err", err)
			return err
		}
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	// Signal active proxy sessions to drain FIRST (cancel the host's base
	// context, which the IPC router roots each proxy pump in). A hijacked
	// WebSocket's request context is not cancelled by srv.Shutdown, so without
	// this the graceful shutdown below would wait out the full 10s grace on every
	// live console. With it, the pumps see ctx.Done() and close, so srv.Shutdown
	// returns promptly — the 10s stays only as an upper bound (REL-02).
	host.BeginShutdown()
	nsCancel() // stop the netstate poll (GO-01/GO-R-05)
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Warn("server shutdown error", "err", err)
	}
	host.Shutdown(shutCtx)
	log.Info("ahv-remote-console runtime stopped")
	return nil
}

func resolveDataDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch currentPlatform() {
	case "windows":
		appdata := os.Getenv("APPDATA")
		if appdata == "" {
			return filepath.Join(home, "AppData", "Roaming", "AHV Remote Console"), nil
		}
		return filepath.Join(appdata, "AHV Remote Console"), nil
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "AHV Remote Console"), nil
	default:
		xdg := os.Getenv("XDG_DATA_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(xdg, "ahvRemoteConsole"), nil
	}
}

func currentPlatform() string {
	// runtime.GOOS is the canonical compile-time platform identifier and
	// matches what the rest of the build system uses ("darwin", "linux",
	// "windows"). Earlier code tried to infer this from $OSTYPE which is
	// not exported by macOS Terminal — that wrongly classified Macs as
	// Linux and put the data dir under $XDG_DATA_HOME instead of
	// ~/Library/Application Support.
	return goruntime.GOOS
}

// loadIPCSecret reads the IPC secret as a hex string (either from stdin or an
// env var) and returns the decoded raw bytes. Both transports use hex because
// it is line-safe for stdin and easy to pass through shell/CI variables; both
// sides of the IPC then use the same raw-byte representation for HMAC and the
// handshake's sha256 hash.
func loadIPCSecret(f *flags) (*ipc.Secret, error) {
	hexStr, err := readSecretSource(f)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("IPC secret is not valid hex: %w", err)
	}
	return ipc.NewSecret(raw), nil
}

func readSecretSource(f *flags) (string, error) {
	switch {
	case f.ipcSecretStdin:
		buf := make([]byte, 4096)
		n, err := os.Stdin.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		line := strings.TrimSpace(string(buf[:n]))
		if line == "" {
			return "", errors.New("empty IPC secret on stdin")
		}
		return line, nil
	case f.ipcSecretEnv != "":
		v := os.Getenv(f.ipcSecretEnv)
		if v == "" {
			return "", fmt.Errorf("env var %q is empty", f.ipcSecretEnv)
		}
		_ = os.Unsetenv(f.ipcSecretEnv)
		return strings.TrimSpace(v), nil
	default:
		return "", errors.New("must specify --ipc-secret-env or --ipc-secret-stdin")
	}
}
