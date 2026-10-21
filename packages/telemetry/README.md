# Telemetry

`@ahv-remote-console/telemetry` is a small metrics and tracing interface for the renderer, with no dependencies.

## Overview

The package keeps a full OpenTelemetry SDK out of the sandboxed renderer bundle.
The renderer emits small structured events to a sink. Forwarding to an OTLP
endpoint is the runtime's job, and only when an operator configures one. See
[`docs/architecture/overview.md`](../../docs/architecture/overview.md) for the
desktop app and runtime.

The package does not send anything by itself. It builds event objects in memory
and passes them to the sink the app installs.

> [!NOTE]
> In the desktop app the sink forwards nothing. It only writes `error*` events to
> the renderer console for local debugging. Runtime telemetry is off by default
> (`telemetry.enabled: false`) and fails validation if you turn it on without
> `telemetry.otlp_endpoint`. With default settings, no telemetry leaves the
> machine.

## Usage

```ts
import { setSink, counter, startSpan, trace } from '@ahv-remote-console/telemetry';

setSink((event) => console.debug(event));
counter('sessions.opened');
const span = startSpan('connect', { vm: '<vm-name>' });
span.end({ ok: true });
await trace('load-resources', () => loadResources());
```

| Export | Purpose |
| --- | --- |
| `setSink(sink)` | Install the function that receives every event. The default drops them. |
| `setGlobalAttributes(attrs)` | Attributes merged into every event. |
| `counter`, `gauge` | Emit a `metric` event with a `value` attribute. |
| `event(name, attrs)` | Emit an `event`. |
| `startSpan(name, attrs)` | Returns a `Span`; `end()` emits a `span` with `duration_ms`. |
| `trace(name, fn)` | Runs an async function in a span and records `ok` or `error`. |
| `TelemetryEvent`, `TelemetrySink`, `EventKind` | Types. |

## Build and test

From the repository root:

```bash
pnpm --filter @ahv-remote-console/telemetry build
pnpm --filter @ahv-remote-console/telemetry typecheck
pnpm --filter @ahv-remote-console/telemetry lint
```

The package has no tests. Entry points are `dist/index.js` and `dist/index.d.ts`.

## Structure

| Path | Contents |
| --- | --- |
| `src/index.ts` | The whole API. |
| `dist/` | Build output. |

## Related documentation

- [Data handling](../../docs/security/data-handling.md)
- [Runtime](../../apps/runtime/README.md)
- [Architecture overview](../../docs/architecture/overview.md)
