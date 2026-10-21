# SSO login window

The SSO window is a separate native window where you sign in to a Prism cluster.
It loads `<prism>/console/` and shows Prism's own login page, so you sign in with
whatever identity provider Prism uses. This page covers the parts we design: the
window shell, its security isolation, and the sign-in states around it. The login
form inside belongs to Prism.

| Part | Source |
| --- | --- |
| Window, partition, certificate trust, cookie watch | [`apps/desktop/src/main/sso-login.ts`](../../apps/desktop/src/main/sso-login.ts) |
| Sign-in panel and its states | [`SignInPanel.tsx`](../../apps/desktop/src/renderer/components/SignInPanel.tsx) |
| Re-authentication from a console | [`ConsoleWindowApp.tsx`](../../apps/desktop/src/renderer/app/ConsoleWindowApp.tsx) |

## Window shell

```text
+-----------------------------------------------------+
| o o o   Sign in to Prism                            |  native title bar
+-----------------------------------------------------+
|                                                     |
|        Prism's own /console/ login page             |  980 x 720
|        (not our design)                             |
|                                                     |
+-----------------------------------------------------+
```

| Property | Value |
| --- | --- |
| Size | 980 x 720, resizable, no min or max |
| Title | "Sign in to Prism" |
| Frame | Native OS frame and controls |
| Background | `#0a0c12` |
| Parent, modal | None. It is a free-standing window |

The window does not load the app's React tree or tokens. Its only colour is the
pre-paint background `#0a0c12`. This near-black is darker than `--hc-bg-0`. It
shows only until Prism paints, avoids a bright flash, and reads as a neutral
shell for a remote page.

## Security isolation

The window handles live Prism credentials, so it is locked down.

| Setting | Effect |
| --- | --- |
| No preload | No context bridge and no IPC. It never sees the IPC secret |
| `contextIsolation: true`, `sandbox: true`, `nodeIntegration: false` | Page code cannot reach Electron or Node |
| `webSecurity: true`, `webviewTag: false`, `spellcheck: false` | Same-origin policy, no nested webviews, no spellcheck on credentials |
| `partition: persist:sso-<endpointId>` | Own cookie jar and storage per endpoint, cache off |
| Permission handlers | Every permission request and check is denied |
| Window open handler | Every `window.open` is denied. Supported identity providers use top-level redirects |

The main process never treats this window as a trusted IPC sender. The app's
strict CSP is not injected here, because it would break Prism's login scripts.

### Certificate trust

The window controls TLS trust only for the cluster host. Identity-provider hosts
use Chromium's default verification.

| Mode | Behaviour |
| --- | --- |
| Pinned | Accepts only the certificate whose SPKI SHA-256 matches the pin |
| Trust on first use (insecure TLS allowed, no pin) | Captures the leaf certificate and fingerprint, accepts it once, and returns it for you to confirm |
| Neither | Chromium default everywhere. A self-signed cluster fails until you mark it insecure |

## Lifecycle

```text
trigger --> open window --> load <prism>/console/ --> Prism and IdP hops
                                                      |
                     +--------------------------------+
                     v                                v
            session cookie seen              you close the window
                     |                                |
         resolve with cookies, close         reject: closed before
                     |                       authentication completed
                     v                                v
         signed in, or certificate confirm      error state
```

- The app strips a trailing slash from the Prism URL before it adds `/console/`.
  A load failure rejects and closes the window.
- The app checks the partition's cookies after each navigation, in-page
  navigation, frame load and the first page load. It treats
  `NTNX_MERCURY_IAM_SESSION`, `NTNX-MERCURY-IAM-SESSION`, `JSESSIONID` and
  `PRISMSESSION` as session cookies.
- A guard settles the result once and closes the window at most once.
- The window opens and closes with the OS default animation.

## Where it opens from

1. **Sign-in panel.** **Sign in with SSO** opens the window. The panel also opens
   it on mount when you reached sign-in through **Connect** or **Add**. A
   background session expiry never opens it, because an unprompted Prism window
   would surprise you.
2. **Console re-authentication.** A `session_expired` error in a console window
   offers **Re-authenticate**. It opens the same window with the endpoint's TLS
   settings, so you sign in again without leaving the console.

## Sign-in panel states

The panel uses the light tool-grade theme: a white 440px card with radius 16 and a
`T.line2` border on a `T.soft` page. The header shows **Sign in**, "Connecting to"
with the endpoint name, and the endpoint URL in mono.

```text
+-------------------------------------------+
| Sign in                                   |
| Connecting to  PROD-CLUSTER               |
| https://prism.example.com                 |  mono
| ----------------------------------------- |
|   A Prism sign-in window will open.       |
|   Complete authentication there...        |
|            [ Sign in with SSO ]           |  T.blue
| ----------------------------------------- |
|            <- Back to endpoints           |
+-------------------------------------------+
```

| State | Trigger | Content |
| --- | --- | --- |
| Idle | Before the window opens | Helper text and **Sign in with SSO** |
| Waiting | Window open | A pulsing blue dot, "Waiting for sign-in…" and helper text |
| Error | Window closed, load failed, no cookies, or Prism rejected the session | "Sign-in did not complete" in `T.red`, the error text, and **Open Prism sign-in again** |
| Certificate confirm | A first-use certificate was captured | "First connection to `<host>`", the SHA-256 fingerprint in mono as colon-separated byte pairs, **Cancel sign-in** and **Trust and sign in** |

- **Trust and sign in** is disabled when no fingerprint is available. The box then
  reads "fingerprint unavailable".
- **Trust and sign in** stores the pin, then sends the session to the runtime.
- **Cancel sign-in** discards the captured session and shows an error. Nothing is
  sent to the runtime.
- **← Back to endpoints** shows in every state.
- The only motion is the waiting dot. It pulses opacity and scale over 1.4s.

## Accessibility

- The native frame gives OS window management and a descriptive window title.
- Prism owns keyboard order, labels and focus inside the page. The app injects no
  styles or CSP that could change them.
- Every panel state pairs colour with text.
- The global reduced-motion rule in `motion.css` shortens the pulse animation to
  a single near-instant run.
- You confirm a first-use certificate by hand, after checking the fingerprint
  out of band. The app never pins it silently.

## Related documentation

- [Connection manager](connection-manager.md)
- [App shell](app-shell.md)
- [Glossary](../reference/glossary.md)
