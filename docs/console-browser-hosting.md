# Hosted Console login

The Billing page can display a temporary, interactive server-side browser for
Claude and OpenAI Console logins. Users need no local helper or browser extension.
Manual session import and the local helper remain available when the broker is
unavailable or a provider rejects the hosted browser.

## Architecture

The separate `console-browser` process runs headed Chromium, Xvfb and x11vnc.
CLIProxy authenticates management requests and proxies the viewer; the broker has
no public ports and receives a separate internal bearer secret. The dashboard
bundles noVNC in its single HTML artifact and receives only a short-lived viewer
ticket, never the provider Console session.

Each login has an independent browser profile and a hard 15-minute lifetime.
There can be at most three simultaneous logins. Finish extracts only the intended
Console session and saves it with the existing billing manager. A successful
save, Cancel, expiry, shutdown or process failure removes the browser and profile.
An incomplete or failed reconnect preserves the previous connection and mappings.

The browser is manually controlled through its display. Chrome DevTools Protocol
is used only on Finish to read the Claude Console cookie or observe OpenAI
dashboard authentication. It does not export Google cookies, retain a Google
profile, or automate entering passwords. Google may still reject hosted logins;
fixture tests and a false `navigator.webdriver` value do not prove acceptance by
Google. Validate sign-in using the intended real account after installation.

## Installation

Build both binaries/UI from matching source, and build the broker image from the
backend repository root:

```sh
docker build -f deploy/console-browser/Dockerfile -t cliproxy-console-browser:local .
```

Create a dedicated random token file on the host, owned by UID/GID `65532:65532`
with mode `0600`. Use at least 32 random bytes encoded as hex. Do not reuse a
provider session or management key. Set `CLIPROXY_BROWSER_TOKEN_FILE` to its
absolute path and merge `deploy/console-browser/compose.example.yaml` after the
existing Compose files. The example targets
the repository service `cli-proxy-api` and preserves its default network. Rename
that service key to match a custom deployment (for example, `cliproxy` on Komodo),
and explicitly retain every network the existing service uses when adding the
`console-browser` network. Existing mounts are retained by the Compose merge.

The backend reads `CLIPROXY_CONSOLE_BROWSER_URL` and
`CLIPROXY_CONSOLE_BROWSER_TOKEN_FILE`. Missing/invalid settings disable hosted
login without affecting inference or existing Console connections. The broker's
flags are `--listen`, `--token-file` and `--profile-root`; the supplied image uses
port 8320, `/run/secrets/console-browser-token` and `/run/console-browser`.

The example grants a container-only init the capabilities needed to install
egress rules and change UID. Init then drops every capability and runs the broker
and Chromium as UID 65532. New egress to private, link-local and metadata ranges
is blocked; public web traffic, loopback, DNS and established reply traffic are
permitted. IPv6 is disabled. The read-only root filesystem, bounded tmpfs profile
storage, resource limits and unpublished VNC/CDP ports are part of this setup.
Do not add a Docker socket, host networking, privileged mode or a public broker
port to work around startup failures.

The broker health check drops to UID 65532 before reading its owner-only token
file; Docker otherwise starts health-check processes with the image default UID. Start and verify it
before enabling the new backend environment. It is optional: a broker outage
must not stop the inference proxy. Deploy the matching management HTML separately.
Back up the current image/HTML and preserve the existing `console-billing` volume
when rolling out or reverting. Browser profiles are temporary; the billing store
continues to own saved Console sessions and mappings.

## Viewer access and lifecycle

Authenticated REST operations live under `/v8/management/billing/browser`.
Viewer tickets expire after 30 seconds, are single use, and are bound to the
browser session and requesting browser Origin. They travel in WebSocket
subprotocols, not query strings. Opening the viewer does not require exposing the
management key to a WebSocket URL. Reconnecting the viewer requires a fresh ticket.

Only the management server can request extraction from the private broker and
save a session. Browser expiry or an unavailable broker does not disconnect an
existing Console account. See [Console billing](console-billing.md) for storage,
manual import, account association and provider endpoint limitations.

## References

- [noVNC API](https://novnc.com/noVNC/docs/API.html)
- [Chrome DevTools Protocol](https://chromedevtools.github.io/devtools-protocol/)
- [Google supported browsers](https://support.google.com/accounts/answer/7675428?hl=en)
