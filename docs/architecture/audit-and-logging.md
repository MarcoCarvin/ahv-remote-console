# Audit log and application logs

This page covers the hash-chained audit log, `audit.log`, and the application
logs of the runtime, the desktop app main process and the agent. It is for
developers and for operators who need to read, check or collect these files.

## How it works

Only the runtime writes the audit log. Each line is one JSON record, and each
record stores the SHA-256 hash of the line before it. Changing or removing a
line in the middle breaks the chain at the next line.

```text
 line 1: {..., "prev_hash": sha256("")}
 line 2: {..., "prev_hash": sha256(line 1)}
 line 3: {..., "prev_hash": sha256(line 2)}
```

The renderer cannot write the file directly. It reaches it through a signed,
allow-listed runtime route. Clipboard text is never recorded.

Each component also writes its own application log:

| Log | Written by | File |
| --- | --- | --- |
| `audit.log` | runtime | `<data-dir>/audit.log` |
| `runtime.log` | runtime (`slog`), captured by the desktop app | `<data-dir>/runtime.log` |
| `main.log` | desktop app main process (`electron-log`) | electron-log folder |
| `hcbridge.log` | agent, inside the guest | per-user data folder in the guest |

Source: [`apps/runtime/internal/audit/audit.go`](../../apps/runtime/internal/audit/audit.go)

## Audit events

There are 30 `Action` constants. Each has one of three states:

- written: a runtime handler appends it.
- allow-list only: `POST /v1/agent/audit` accepts it, but no runtime code
  appends it.
- defined, never written: the constant exists and nothing appends it.

| Action | State | Written by |
| --- | --- | --- |
| `runtime.start` | written | runtime startup |
| `runtime.stop` | written | runtime shutdown (deferred) |
| `plugin.load_failure` | written | startup, once, if plugin loading fails |
| `endpoint.upsert` | written | `upsertEndpoint` |
| `endpoint.delete` | written | `deleteEndpoint` |
| `auth.start` | written | `pluginAuthStart` |
| `auth.complete` | written | `pluginAuthComplete` |
| `auth.refresh` | written | `pluginAuthRefresh` |
| `auth.logout` | written | `pluginLogout` |
| `session.open` | written | `openSession` |
| `session.close` | written | `closeSession` |
| `diagnostics.export` | written | `diagnosticsBundle`, success and failure |
| `vm.snapshot.restore` | written | `vmRestoreSnapshot`, every attempt |
| `bridge.install` | written | `agentMountISO`, `mountISO`, `ejectISO` |
| `presence.join` | written | `presenceWS` |
| `presence.leave` | written | `presenceWS` |
| `bridge.uninstall` | allow-list only | |
| `bridge.connect` | allow-list only | |
| `bridge.disconnect` | allow-list only | |
| `bridge.clip_in` | allow-list only | |
| `bridge.clip_out` | allow-list only | |
| `bridge.probe_reject` | allow-list only | |
| `clipboard.host_to_vm` | defined, never written | |
| `clipboard.vm_to_host` | defined, never written | |
| `session.stream.open` | defined, never written | |
| `session.stream.close` | defined, never written | |
| `auth.invalidate` | defined, never written | |
| `updater.apply` | defined, never written | |
| `presence.role_change` | defined, never written | |
| `vm.snapshot.create` | defined, never written | `vmCreateSnapshot` does not append |

Totals: 16 written, 6 allow-list only, 8 defined and never written.

These handlers write no audit record:

- read-only polls such as `listSessions`, `vmStats` and `listTasks`, left out on
  purpose to avoid flooding the log
- VM power actions (`vmPowerAction`)
- serial-port changes (`vmAddSerialPort`)

Source: [`apps/runtime/internal/ipc/router.go`](../../apps/runtime/internal/ipc/router.go)

## Record format

Each record is one JSON object from `json.Marshal`, followed by one `\n`, in
UTF-8. The fields come from the `Record` type.

| Key | Set by | Notes |
| --- | --- | --- |
| `seq` | logger | starts at 1 |
| `ts` | logger | `time.Now().UTC()`, RFC 3339 with nanoseconds |
| `actor` | logger | OS user of the runtime process, read once at `Open` |
| `action` | caller | one of the actions above |
| `endpoint_id` | caller | omitted when empty |
| `session_id` | caller | omitted when empty |
| `vm_id` | caller | omitted when empty |
| `plugin_id` | caller | omitted when empty |
| `result` | caller | defaults to `"ok"` |
| `reason` | caller | omitted when empty, passed through `logging.Scrub` |
| `extra` | caller | omitted when empty; free-form map, not scrubbed |
| `prev_hash` | logger | lower-case hex SHA-256, always present |

`json.Marshal` escapes `<`, `>` and `&`. The hash covers the bytes as stored. A
verifier must hash the stored bytes and must not re-serialize the JSON.

## Chain verification

### Linkage

- The first record's `prev_hash` is the SHA-256 of an empty input:
  `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`.
- Record N's `prev_hash` is the SHA-256 of line N-1, without its trailing `\n`.
- A record does not store its own hash. Only the next record holds it.
- `Append` holds a mutex for the whole write.

### Startup does not verify the chain

At startup, `recover` reads the whole file but parses only the last non-empty
line. It takes `seq` and `prev_hash` from that line. If the line does not
parse, `Open` fails and the runtime does not start. Damage to earlier lines
shows up only when you verify the chain.

### Verifier algorithm

`VerifyChain` is the reference implementation:

1. Read the file as bytes. Set `expected` to the SHA-256 of an empty input.
2. Split at each `\n`. Skip empty segments. Ignore a final segment that has no
   `\n`.
3. Parse the segment as JSON. If its `prev_hash` differs from `expected`,
   report its `seq` as the first bad record.
4. Set `expected` to the SHA-256 of the segment bytes, and continue.

It returns `(0, nil)` when the chain is intact, `(seq, error)` on a mismatch,
and `(rec.Seq, error)` on a parse failure, where `rec.Seq` can be 0.

`VerifyChain` is in the `internal/audit` package and only the tests call it.
There is no CLI command or script to verify a file, and the runtime has no
verify flag. Because the package is under `internal/`, only code inside the
runtime module can call it. Unit tests cover restart continuity and tamper
detection.

## Storage

### Location and permissions

The audit log is `<data-dir>/audit.log`. The data directory comes from
`--data-dir`, which the desktop app always passes, or from these defaults:

| OS | Default data directory |
| --- | --- |
| Windows | `%APPDATA%\AHV Remote Console` |
| macOS | `~/Library/Application Support/AHV Remote Console` |
| Linux and other | `$XDG_DATA_HOME/ahvRemoteConsole` (default `~/.local/share/ahvRemoteConsole`) |

The runtime creates the data directory with mode `0700`. `Open` creates the
parent `0700` and opens the file `O_APPEND|O_CREATE|O_WRONLY` with mode `0600`.
It then sets `0600` on an existing file too. A failed `chmod` is only logged as
a warning.

### fsync policy

`Append` calls fsync after a write unless the action is one of these frequent,
low-value events:

`clipboard.host_to_vm`, `clipboard.vm_to_host`, `bridge.clip_in`,
`bridge.clip_out`, `bridge.connect`, `bridge.disconnect`,
`session.stream.open`, `session.stream.close`, `presence.join`,
`presence.leave`, `presence.role_change`.

All other actions, including unknown ones, are synced. The next synced write
flushes the whole file, and `Close` syncs too. A hard crash can lose deferred
records written since the last sync. The chain stays valid, because those lines
were never acknowledged.

On failure:

- A failed `Sync` is logged as a warning, and the chain continues.
- A failed `Write` is logged, not returned. The `seq` number is already used and
  `prev_hash` does not advance, so the chain stays valid but `seq` has a gap.

### Turning the audit log off

Set `security.audit_trail_enabled` in `<data-dir>/config.json`. The default is
`true`. The runtime reads it once at startup. When it is `false`, no file is
created and `Append` returns at once.

### Rotation and retention

The audit log is never rotated. No code renames, truncates, deletes or caps the
file, and there is no retention period or archive step. `recover` and
`VerifyChain` read the whole file each time. If you start a new file, it starts
a new chain from the genesis value.

The diagnostics bundle includes the last 256 KiB of `audit.log`.

## What is not recorded

### Clipboard text

Any record about a clipboard transfer may carry only byte length, direction
and SHA-256. Clipboard audit records are not written in this release, so no
written record exercises this rule today. The code holds it in layers:

1. The renderer type `ClipboardMeta` has no text field. `ClipboardAuditEmit`
   accepts only that type.
2. The runtime `Record` has no clipboard field.
3. The agent audit request body, `agentAuditRequest`, has five fields. `readJSON`
   rejects unknown fields, so an extra `text` field gets a 400.
4. `agentAuditExtra` writes only `bytes`, `direction` and `sha256`.
5. `Reason` goes through `logging.Scrub`, which removes cookie and auth-header
   patterns but not arbitrary text.
6. The agent logs message type and payload length, never the payload.

### Credentials and identity

- Cookies and bearer tokens are scrubbed from `Reason` by pattern. No code
  writes passwords or tokens into `extra`.
- `actor` is the runtime's OS user. The operator's Prism or SSO identity is not
  recorded.

## Agent audit records

The agent in the guest has no connection to the runtime. Records about it come
from the desktop side:

1. The SDK method `recordAgentAudit` posts
   `{action, sessionId, bytes?, direction?, sha256?}` to `POST /v1/agent/audit`.
2. The runtime handler `agentAudit` checks the action against the allow-list,
   builds `extra`, calls `Append`, and returns 204. `Append` returns no error,
   so the caller never learns whether the record was written.

How far these records can be trusted:

- The route is behind the HMAC layer. The desktop app main process signs the
  request.
- The runtime does not check that `sessionId` names a real session, or that
  `bytes`, `direction` or `sha256` describe a real transfer. `direction` is not
  validated. Whoever holds the IPC secret can write any bridge record.
- `actor` is the runtime's OS user, not the guest or the renderer.
- `bridge.probe_reject` records are claims by the caller, not observations by
  the runtime.

The agent itself writes no audit records. Its diagnostics go to its own log.

Source: [`packages/sdk/src/index.ts`](../../packages/sdk/src/index.ts)

## Application logs

### Runtime (`slog`)

| Setting | Flag | Default | Values |
| --- | --- | --- | --- |
| Level | `--log-level` | `info` | `debug`, `info`, `warn`/`warning`, `error`/`err` |
| Format | `--log-format` | `json` | `json`, `text` |
| File | `--log-file` | none | path; stderr is always written |

The desktop app starts the runtime with `--log-format json --log-level info` and
no `--log-file`. It appends the child's stdout and stderr to
`<data-dir>/runtime.log`. No code rotates or truncates `runtime.log`.

Redaction runs in the handler's `ReplaceAttr`:

1. A value under a key from the sensitive-key list, or under a key containing
   `secret`, `password` or `token`, becomes `***redacted***`.
2. Every string value is scanned, whatever its key, for Cookie, Set-Cookie,
   Authorization and Proxy-Authorization values, Bearer and Basic tokens, and
   Nutanix session cookies (`NTNX_*SESSION`, `JSESSIONID`, `PRISM_SESSION`,
   `igw_session`). The secret part becomes `[REDACTED]`.
3. `Scrub` applies the same patterns to free text.

Redaction works by pattern only. A secret in another format, or under a key
without a matching word, is not caught.

Source: [`apps/runtime/internal/logging/logging.go`](../../apps/runtime/internal/logging/logging.go)

### Desktop app main process (`electron-log`)

- Levels. The file gets `info`. The console gets `debug` in development and
  `warn` in production.
- Location. The repo does not set it. The electron-log default is `main.log`
  in the app's logs folder: under `%APPDATA%` on Windows, `~/Library/Logs` on
  macOS, `~/.config` on Linux. The app reports the actual path through
  `log.transports.file.getFile().path`.
- Rotation. The library default: at 1 MiB the file is renamed to
  `main.old.log`.
- Renderer lines. The renderer sends lines over the `hc-log` channel.
  `handleHcLog` checks the sender and caps the tag at 64 and the message at
  2048 characters. Control characters become spaces to stop line injection.
  Secrets are not removed, and the runtime's redaction does not run here.
- Diagnostics. The last 4 MiB of `main.log` and `runtime.log` are collected.

### Guest agent (`hcbridge.log`)

| OS | Path |
| --- | --- |
| Linux | `$XDG_DATA_HOME/ahv-remote-console/hcbridge.log` (default `~/.local/share/...`) |
| Windows | `%LOCALAPPDATA%\AHV Remote Console\hcbridge.log` |
| other | no file |

The file is opened `0600` in append mode. The default level is `debug`, marked
in the code as temporary. When the file passes 5 MiB it is truncated to empty
and the old lines are lost. The handler does no redaction. Secrets stay out
because the agent logs metadata only.

## Operator tasks

### Find the files

`audit.log`, `runtime.log` and `config.json` are in the data directory (see
[Location and permissions](#location-and-permissions)). `main.log` is in the
electron-log folder for your OS. `hcbridge.log` is inside the guest.

### Read and check the audit log

Each line is one JSON object, so you can filter on `action` or `result` with
any JSON tool. No verify command ships. To check the chain, follow the
[verifier algorithm](#verifier-algorithm), or write a short Go test under
`apps/runtime` that calls `audit.VerifyChain` on a copy of the file.

### Collect logs for support

1. In the desktop app, choose **Export Diagnostics...**.
2. The zip holds the runtime bundle (`config.json` with pinned certificates
   blanked, the tails of `audit.log` and `runtime.log`, `runtime-state.json`)
   and `host-logs/` (`main.log`, `runtime.log`, `about.txt`).
3. If the runtime is unreachable, the zip holds only the host logs.

**Open Log File** shows `main.log`.

### Handle audit files

- Copy `audit.log` only while the runtime is stopped.
- Do not edit, delete or move it while the runtime runs.
- A new file starts a new chain. Verify each file on its own.
- If startup fails because the last line does not parse, there is no repair
  command, and a hand repair breaks the chain.

### Turn recording off

Set `security.audit_trail_enabled` to `false` in `config.json` and restart the
runtime.

## Known gaps

### Audit chain

- No external anchor. The newest hash is stored nowhere else, so deleting
  the last N records leaves a valid chain.
- A whole-file rebuild is not detected.
- Tail edits can be absorbed. If the last line is changed and the runtime
  restarts, `recover` chains new records onto the changed bytes.
- `seq` is not checked. Gaps and duplicates pass `VerifyChain`.
- No file lock. Two runtimes on one data directory would interleave
  writes.
- No rotation. `audit.log` grows without limit. A code comment names
  rotation as the fix for size, but none exists.

### Records that are defined but not written

- Clipboard records (`clipboard.host_to_vm`, `clipboard.vm_to_host`) are never
  appended. The renderer's audit emitter defaults to a no-op, and the console
  view passes no audit option.
- Nothing in the repo calls `recordAgentAudit` outside the SDK and its tests,
  although the SDK comment says the renderer's `useAgent` posts these records.
  The [protocol reference](../reference/protocol.md) describes bridge record
  contents.
- The package comment in `audit.go` lists "autoUpdater runs" as audited, but
  `updater.apply` is never written.
- The `result` value `"denied"` is documented in a code comment but no call site
  uses it. Another comment shows `"extra":{}`, which `omitempty` never
  produces.
- VM power actions and serial-port changes are not audited, and the code gives
  no reason.

### Logs

- `runtime.log` is never rotated or truncated. A comment in `logging.go`
  mentions a log-file rotation hook that does not exist.
- The `hc-log` channel accepts renderer text up to 2048 characters. Keeping
  clipboard errors to size and reason only is a code convention, not a check.

### Not verified

- The Go tests for `audit` and `logging` were not run for this page.
- On Windows, `os.Chmod` only changes the read-only attribute. The effective ACL
  on `audit.log` is not verified.
- Partial writes at the OS level, two runtimes on one data directory, and
  rotating `audit.log` by hand while the runtime runs.
- The packaged electron-log path.
- Whether the production renderer emits bridge records.
- Whether the settings UI exposes `audit_trail_enabled`. The repo does not
  document it as an operator control.
- Whether `slog`'s `ReplaceAttr` also receives the built-in `msg` attribute in
  the pinned Go version.
- The diagnostics zip contents were read from source. No bundle was generated.

## Related documentation

- [Clipboard](clipboard.md)
- [In-guest agent](agent.md)
- [Data handling](../security/data-handling.md)
- [Threat model](../security/threat-model.md)
- [Troubleshooting](../operations/troubleshooting.md)
- [Configuring](../operations/configure.md)
