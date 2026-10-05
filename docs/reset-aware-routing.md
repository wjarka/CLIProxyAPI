# Reset-aware account routing

This fork adds `routing.strategy: earliest-reset` and an embedded account dashboard
at `/quota.html`. Existing upstream account management stays at `/management.html`.
The dashboard uses the authenticated `/v8/management/credentials/quota-overview`
endpoint. Its management key lives only in browser memory and is cleared on
reload or Disconnect; no provider tokens are returned by the overview endpoint.

## Start locally

Build with Go 1.26 or later:

```sh
go build -o cli-proxy-api ./cmd/server
cp examples/reset-aware/config.yaml config.yaml
# Replace BOTH example secrets with independent random values before starting.
./cli-proxy-api --config config.yaml
```

Open `http://127.0.0.1:8317/management.html` to add accounts, then
`http://127.0.0.1:8317/quota.html` for the overview. Use the management secret to
connect the dashboard and the client secret for inference clients.

For a Linux service, build with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` (or `arm64`
for ARM hardware), or use the upstream Dockerfile. The example binds only to
loopback. A central deployment must deliberately configure a private listener or
reverse proxy, TLS, firewall access and `management.allow-remote` if needed.
Keep API authentication enabled. Preserve WebSocket upgrades in the reverse proxy.
No deployment or client credentials are changed by this fork.

## Selection behavior

1. Existing availability checks exclude disabled, expired or cooling credentials.
2. Explicit credential priority and Codex WebSocket capability still take precedence.
3. Among eligible accounts, choose the soonest **future weekly quota reset**.
4. Established session bindings stay on their account, even if reset rankings change.
   Upstream session affinity performs failover when the bound account is unavailable.
5. If no candidate has a usable reset observation, use round-robin.

Claude uses `anthropic-ratelimit-unified-7d-reset`; Fable requests prefer its
`7d_oi` reset when supplied. Codex uses whichever primary/secondary window reports
10080 minutes, accepting absolute reset timestamps or relative seconds anchored
to the observation time. Five-hour resets are displayed but do not drive ranking.
Snapshots older than 24 hours, malformed data and elapsed resets are ignored.
This changes selection, not upstream quota enforcement or cooldown handling.

Quota observations come from upstream responses, including Codex WebSocket quota
events. They are **passive**, not provider polling. Newly added accounts have no
known reset until used; known-reset candidates outrank unknown candidates. An
account that has never been used may therefore remain unobserved while known
accounts are available. Use each new account once through the proxy before relying
on a complete ranking. Dashboard Refresh reloads these observations; it does not
send inference or probe provider accounts. After a provider grants an unscheduled
reset, the next observed response updates the ranking.

## Codex WebSockets

Upstream already implements the WebSocket transport. Both ends must enable it:

- In the existing Management UI, set the Codex credential's `websockets` field to
  `true` (the same top-level field is supported in its OAuth auth JSON). The quota
  dashboard reports the observed credential configuration, not a connection test.
- The Codex client's custom provider must support/use Responses WebSockets
  (`supports_websockets = true` in its provider entry, with `wire_api = "responses"`).
  Point its base URL to the proxy's `/v1` and supply the proxy client key through
  the client's environment-key configuration.
- Any reverse proxy must forward WebSocket upgrade requests to `/v1/responses`.

The proxy selects its WebSocket executor only for downstream WebSocket requests
with a WebSocket-enabled credential. HTTP clients continue to use HTTP; setting
one side alone does not convert the connection. Provider login and a real end-to-end
inference test are needed to verify your deployment.

## Development checks

```sh
go test ./sdk/cliproxy/auth ./sdk/cliproxy ./internal/api ./internal/api/handlers/management
go build -o test-output ./cmd/server
```

Keep this patch on a feature branch so upstream can be merged independently.
The selector uses the custom-selector path rather than the optimized round-robin
scheduler, because its ordering depends on live quota observations.

Codex provider settings reference:
https://developers.openai.com/codex/config-reference/
