# Console billing connections

The management UI's **Billing** page connects Claude and OpenAI Console accounts separately from inference credentials. Connect three independent logins as three named connections, or use a single login that can read several organizations. Accounts are identified by provider and organization ID; several API keys can share one account and balance.

Console observations now participate in the configured routing strategy. Under
`earliest-reset`, verified API organizations are ranked by their earliest future
credit grant expiration with a known positive remaining amount. Ordinary model
availability, cooldowns and credential priority are evaluated first. Round-robin,
fill-first and weighted strategies retain their existing behavior. This does not
add routing tiers, dollar reserves or balance-based quota protection.

## Connect a Console

When hosted login is configured, **Billing → Connect Console** offers an
in-dashboard browser. Choose a provider and connection name, start the browser,
sign in to the intended Console, then click **Finish**. Reconnect follows the
same flow while retaining the existing connection ID and mappings. Cancel or
an incomplete login does not replace a working connection. Temporary browsers
expire after 15 minutes. See [Hosted Console login](console-browser-hosting.md)
for broker installation and compatibility limits.

### Local helper and manual fallback

If hosted login is unavailable, use manual session import or the local helper:

1. Open **Billing → Connect Console**, choose Claude or OpenAI and enter a descriptive label.
2. Download the login helper from that page. The download uses your existing management authentication.
3. Run the copied command locally, for example:

   ```sh
   uv run ./console-login.py --provider claude --url https://proxy.example --label Personal
   uv run ./console-login.py --provider openai --url https://proxy.example --label Work
   ```

4. Enter your CLIProxyAPI management key at the hidden terminal prompt. Sign into the intended provider account in the browser window and open Billing. Press Enter in the terminal when ready.
5. Reload the Billing page. Repeat for each independent login. Each helper invocation opens an isolated, temporary browser context; it does not reuse your normal browser's cookies or save a browser profile.

The helper requires Python 3.11+, uv, and Chrome. It uses Playwright Chromium if Chrome is unavailable; install that browser with `uv run --with playwright playwright install chromium`. The inline dependency declaration lets uv supply Playwright without a global Python installation. `CLIPROXY_MANAGEMENT_KEY` is an alternative to the hidden key prompt; do not put a management key in command arguments. Uploads require HTTPS except for loopback HTTP.

Claude uses the Console `sessionKey` cookie. OpenAI captures a dashboard Bearer session from an observed request to a fixed provider origin. Ordinary inference API keys are rejected as Console sessions. The management key is uploaded only to your proxy, and provider sessions are uploaded in the authenticated request body, never URL parameters. The UI also offers a masked manual-session import; it does not persist that value in browser storage.

Console sessions can expire and have no supported automatic refresh flow here. Choose **Reconnect** and run the downloaded helper with the copied `--connection-id` command. A reconnect keeps the connection ID, organization accounts and key mappings, and rejects a login that no longer contains its previous organizations. If account membership intentionally changes, add a separate connection and explicitly rebind affected keys.

**Disconnect** removes the stored login session. It retains the connection row, organization identity, mappings and previous balance marked stale. It is safe to reconnect later. A Console authentication error does not disable an inference API key or log you out of CLIProxyAPI management.

## Link API keys

Eligible credentials appear below the organizations. Claude API keys must use the default first-party Anthropic endpoint. OpenAI API keys must explicitly target `https://api.openai.com` or its `/v1` base; subscription OAuth credentials and third-party compatible gateways are excluded.

To distinguish inference keys, edit a credential in **AI Providers** and set its optional **Key name**. OpenAI-compatible groups have a separate name for each key entry. Billing displays these names; clearing one restores the default label. Renaming preserves the credential ID, session affinity, Console mapping, and verification. In YAML, set `label` on the individual entry under `api-keys.<provider>[].keys[]`, separately from the group `name`.

Choose an organization belonging to the same provider and click **Link**. Manual associations are marked **Unverified**. **Verify organization** sends a read-only `GET /v1/models` using the inference API key to its fixed first-party origin and checks the organization response header against a connected Console account. It does not send a model inference request or the Console session. If the provider omits that header, verification fails and the explicit unverified mapping remains available.

OpenAI verification honors fixed `OpenAI-Organization` and `OpenAI-Project` credential headers. Dynamic selectors and conflicting authentication overrides cannot be verified. Verification is bound to a fingerprint of the credential and its custom headers; changing those settings makes the UI report it as unverified until verified again, including after a server restart. Fingerprints are not included in management responses. Manual relinking always resets verification.

## Balance meaning and freshness

- A balance is the last observed USD credit balance, not a subscription usage limit. Missing amounts remain unknown; a reported zero remains zero.
- Grant amounts, remaining credits and expiration dates are shown only when provided. Expired grants remain visible. A missing expiration is not treated as unlimited or as an upcoming reset.
- The service refreshes active Console connections at startup and every 60 seconds,
  independently of whether Billing or Quota Management is open. Transient failures
  use bounded exponential backoff; expired sessions wait for reconnect. **Refresh
  balance** still fetches on demand. After five minutes, observations become stale
  and cease to influence grant ranking.
- Failed requests preserve the last observation and show a safe error. A multi-organization login can expose readable organizations while showing denied organizations as unknown or stale. A successful observation from another connection is retained.
- No fresh balance or usable credits are inferred from an expired session, permission denial, unsupported response, or unknown field.

## Grant selection and session affinity

Select `earliest-reset` in the existing routing configuration:

```yaml
routing:
  strategy: earliest-reset
```

Under `earliest-reset`, a verified API key uses its organization's earliest future
expiration among grants with a known positive remaining amount. Exhausted, expired,
unknown and invalid grant values do not supply a date. When the next successful
observation reports that grant used up, the organization's next remaining grant
becomes its date and another eligible organization can take over. Provider reports
are observed rather than estimated from token usage, so the change is detected on
refresh, not at the exact moment of a debit.

For example, A with remaining grants expiring November 11 and December 11 ranks
ahead of B expiring November 15. After A's November 11 grant is reported exhausted,
B ranks ahead of A's December 11 grant. Sessions follow that change. Within a
still-winning organization, or when dates tie, an eligible existing key binding
stays sticky. Explicit session IDs, parent aliases and content-prefix affinity
use the same rule. OAuth affinity and other strategies retain their existing rules.

Only verified links matching the current key, endpoint and custom headers can
supply grant dates. Renaming a key preserves verification. Unlinking, replacing a
key, disconnecting a login or losing fresh billing access removes its grant
preference. Unknown or stale organizations remain ordinary routing candidates;
Console failures do not disable inference keys. A known empty aggregate balance
also removes the grant preference rather than introducing a new blocking policy.
Fresh authoritative grants can still rank an organization whose aggregate balance
is unknown. If no usable date exists, the strategy uses its existing fallback.

Scheduling reads an immutable in-memory view: inference requests never wait for
Console network calls or encrypted-store reads. Management and background refresh
share one store, and committed updates replace that view. Shutdown cancels and
drains automatic refreshes before retiring it. If the service cannot open its
billing store at startup, inference continues and Billing reports unavailable;
fix the storage issue and restart to restore the shared store and polling.
Background work is bounded to four concurrent requests and follows the server's
existing policy of no added network timeouts. One stalled request does not block
other free slots; if every slot stalls, polling waits for a response or shutdown.
The five-minute freshness limit still retires old routing preferences.

## Persistence and deployment

Billing state lives in `<directory-containing-config.yaml>/console-billing/`. The directory must be writable by the server and must survive container replacement. If only the configuration file is mounted read-only, mount a separate writable directory at the adjacent `console-billing` path. Home mode disables these management endpoints.

The directory is owner-only (`0700`). `billing.enc` holds the AES-GCM encrypted sessions, snapshots and bindings; `session.key` is an owner-only (`0600`) local encryption key. Keep both together in a restricted backup. Encryption does not protect against someone who can read both files or act as the server user. Losing the key requires new Console logins. Corruption fails closed rather than overwriting the store. Multiple server processes must not actively share this sidecar: writes detect conflicts and require a restart/reload of the stale process before retrying.

Deploy the matching management UI build to obtain `#/billing`. Updating this backend alone does not add that page to an older UI artifact. No production deployment is part of this change.

## Provider compatibility

Console billing endpoints are undocumented and may change independently of supported inference APIs. The implementation uses fixed origins and bounded, read-only requests, refuses redirects, and reports an unsupported endpoint/schema instead of inventing a balance or organization. Fixture tests validate parsing, persistence and management behavior. Live logins and balances have not been validated with real accounts.

Claude adapter evidence is the [CodexBar Console guide](https://github.com/steipete/CodexBar/blob/main/docs/claudeplatform.md) and its referenced provider source. OpenAI's [official API overview](https://developers.openai.com/api/reference/overview) documents organization/project request selection; the legacy dashboard credit shape is corroborated by the [dsh-api-balance implementation](https://github.com/GPIOX/dsh-api-balance/blob/main/lib/index.js). OpenAI `/v1/me` organization discovery is documented for ordinary API keys; compatibility with the browser dashboard session remains unverified. This is not an official public billing-balance API integration.

The v8 HTTP contract is in [management-api-v8.md](management-api-v8.md).
