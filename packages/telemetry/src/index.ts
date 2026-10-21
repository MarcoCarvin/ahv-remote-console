/**
 * @ahv-remote-console/telemetry — minimal, dependency-free metrics + tracing facade.
 *
 * Renderer telemetry needs to be tiny and not pull a full OTEL SDK into the
 * sandboxed bundle. The runtime is responsible for forwarding to an OTLP
 * endpoint when one is configured; the renderer just emits structured events.
 */

export type EventKind = 'metric' | 'event' | 'span';

export interface TelemetryEvent {
  kind: EventKind;
  name: string;
  ts: number;
  attrs?: Record<string, string | number | boolean>;
  /** For span events. */
  duration_ms?: number;
}

export type TelemetrySink = (event: TelemetryEvent) => void;

let sink: TelemetrySink = () => undefined;
let globalAttrs: Record<string, string | number | boolean> = {};

export function setSink(s: TelemetrySink): void {
  sink = s;
}

export function setGlobalAttributes(attrs: Record<string, string | number | boolean>): void {
  globalAttrs = { ...globalAttrs, ...attrs };
}

export function counter(
  name: string,
  value = 1,
  attrs?: Record<string, string | number | boolean>,
): void {
  sink({ kind: 'metric', name, ts: Date.now(), attrs: { ...globalAttrs, ...attrs, value } });
}

export function gauge(
  name: string,
  value: number,
  attrs?: Record<string, string | number | boolean>,
): void {
  sink({ kind: 'metric', name, ts: Date.now(), attrs: { ...globalAttrs, ...attrs, value } });
}

export function event(name: string, attrs?: Record<string, string | number | boolean>): void {
  sink({ kind: 'event', name, ts: Date.now(), attrs: { ...globalAttrs, ...attrs } });
}

export interface Span {
  end(extra?: Record<string, string | number | boolean>): void;
}

export function startSpan(name: string, attrs?: Record<string, string | number | boolean>): Span {
  const start = performance.now();
  return {
    end(extra) {
      sink({
        kind: 'span',
        name,
        ts: Date.now(),
        duration_ms: performance.now() - start,
        attrs: { ...globalAttrs, ...attrs, ...extra },
      });
    },
  };
}

/**
 * Decorator-style helper for async functions. Captures duration + error.
 */
export async function trace<T>(
  name: string,
  fn: () => Promise<T>,
  attrs?: Record<string, string | number | boolean>,
): Promise<T> {
  const span = startSpan(name, attrs);
  try {
    const result = await fn();
    span.end({ ok: true });
    return result;
  } catch (err) {
    span.end({ ok: false, error: (err as Error).message });
    throw err;
  }
}
