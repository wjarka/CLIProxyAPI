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

For accounts without reserve policies, quota observations come from upstream
responses, including Codex WebSocket quota events. They are **passive**, not provider polling. Newly added accounts have no
known reset until used; known-reset candidates outrank unknown candidates. An
account that has never been used may therefore remain unobserved while known
accounts are available. Use each new account once through the proxy before relying
on a complete ranking. For unrestricted accounts, Dashboard Refresh reloads these observations; it does not
send inference or probe provider accounts. After a provider grants an unscheduled
reset, the next observed response updates the ranking.

## Protected weekly budgets

Protect accounts shared with Claude Desktop or other direct clients using named
policies. Percentages are points of the account's entire weekly allowance, per
scheduled day. Each budget is protected in advance and released entirely at its
local deadline; there is no gradual release or attribution of external usage.

```yaml
routing:
  strategy: earliest-reset
  reserve-policies:
    work-account:
      timezone: Europe/Warsaw
      schedule:
        - days: [mon, wed, thu, fri]
          protected-budget-percent: 10
          release-at: "17:00"
        - days: [tue]
          protected-budget-percent: 20
          release-at: "17:00"
  account-policies:
    - account: "<exact credential ID from quota-overview>"
      reserve-policy: work-account
```

The reserve is the sum of future release deadlines up to the account's observed
weekly reset. For a Wednesday 05:00 reset, it falls from 60 points Wednesday
morning to 50 Wednesday at 17:00, 40 Thursday, 30 Friday, 20 Monday and zero
Tuesday at 17:00. Calendar dates honor the policy timezone and daylight saving.
Changing the provider's reset timestamp automatically changes the cutoff; the
configured Tuesday budget itself remains Tuesday-specific.

The manager excludes accounts whose remaining balance is at or below the reserve
before applying priority or session affinity. This also applies to explicit
credential selection, mixed routes, retries and plugin delegation. Account prefixes
and upstream cooldowns retain their existing meaning. Unassigned accounts are
unrestricted. If all matching credentials are protected, requests return HTTP 429
with `quota_reserved`; explicit work routes do not escape into another account pool.

Protected Claude and Codex accounts use their authenticated read-only usage
endpoints when their shared weekly observation is missing, invalid, expired or
older than one minute. Concurrent refreshes are shared. Claude shares per-account
429 backoff with the management quota UI. Failed Codex reads are retried at most
once every 15 seconds. Waiting for quota metadata during credential acquisition
is bounded to 500 ms. Provider reads continue in the manager lifetime and are
canceled at shutdown.

If a refresh is unavailable, routing can keep using a valid weekly observation
up to 30 minutes old, until the observed weekly reset. The same reserve floor
applies to the last known remaining balance, and the management reserve status
marks the observation as stale. Once the grace expires, or if no valid
observation exists, the account remains protected until a valid read completes.
Active reserve observations are separate from passive plan, credit and model
observations. No inference probe is sent. Direct clients can consume below the reserve, and data
lag and already-running requests mean this is a routing threshold rather than an
exact spending cap. The policy only protects the shared weekly pool; it does not
reserve short-session or separate model-specific limits.

`quota-overview` and the embedded dashboard show the policy, protected balance,
next release and whether the account is reserved or waiting for quota data.
Dashboard refresh may perform the same cached read-only usage check for protected
accounts. Policy edits work through the existing v8 config API and hot reload.
Invalid timezones, weekdays, release times, budgets, weekly totals above 100,
duplicate account bindings and references to missing policies are rejected.
Reserve policies require standalone execution. A Home-mode service rejects this
combination at startup; Home dispatch also refuses it after a runtime update.

## Working-hours five-hour guard

A separate named usage policy protects part of the five-hour quota during
configured local working hours, using the weekly reserve's protected-budget convention:

```yaml
routing:
  usage-policies:
    work-hours:
      timezone: Europe/Warsaw
      protected-budget-percent: 30
      schedule:
        - days: [mon, tue, wed, thu, fri]
          start: "09:00"
          end: "17:00"
  account-policies:
    - account: work-claude.json
      reserve-policy: work-account # Existing weekly reserve, defined above.
      usage-policy: work-hours
```

Account bindings may specify either policy or both. The guard protects 30% of the
five-hour quota, blocking at 70% consumed, independently of weekly balance. At 09:00
it becomes active; at 17:00 it stops applying. Weekends are unrestricted by this
guard. Timezone rules include daylight saving time. Schedule entries are same-day
intervals with `HH:MM` start before end; use multiple entries for split workdays.
Protected percentages range from 0 through 100, inclusive; 0 reserves nothing,
while 100 blocks all selections during the schedule. Missing or invalid quota
data still fails closed. Unknown policy references and invalid schedules fail validation.
The initial `five-hour-used-percent` key is accepted on input for upgrades and
converted to protection (`70` becomes `30`); specifying both keys is rejected.
New configuration and status responses use protected percentages.

During protected hours, stale (older than one minute), missing, invalid or expired
five-hour observations block selection until a read-only refresh succeeds. Fresh
weekly data cannot substitute for five-hour data. Claude uses the shared `5h`
window; Codex identifies the window by its 300-minute duration. Outside protected
hours the guard requires no quota observation; any weekly reserve and upstream
cooldown still apply. Both guards share cached, deduplicated metadata reads and
the existing bounded credential-acquisition wait. Direct provider use is unaffected.

Pinned credentials, session affinity, retries, mixed-provider routes and scheduler
plugins cannot bypass the guard. When it excludes all matching credentials,
requests receive HTTP 429 with `quota_usage_guard` (weekly reserve failures retain
`quota_reserved`). The quota overview exposes a separate `usage_policy` object:
schedule, timezone, `protected_percent`, `remaining_percent`, active interval end,
observed consumption/reset,
blocked flag and reason (`five_hour_limit` or `quota_unknown`). Configuration edits
use the validated v8 management API and hot reload, alongside existing reserves.
Home dispatch is unsupported and rejected for either quota policy.

This is a routing threshold, not a hard spending cap: cached provider readings,
concurrent requests and already-running streams can exceed the threshold before
a later selection observes the new consumption. Existing streams are not aborted.

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
