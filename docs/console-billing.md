# Console billing connections

The management UI's **Billing** page connects Claude and OpenAI Console accounts separately from inference credentials. Connect three independent logins as three named connections, or use a single login that can read several organizations. Accounts are identified by provider and organization ID; several API keys can share one account and balance.

This release supplies balances, credit grants and expiration dates, connection health, and API-key association. It does not yet apply billing balances to credential selection, dollar reserves, earliest-expiration scheduling, or routing tiers. Existing priority, strategy, session affinity and quota protection behave as before.

## Connect a Console

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

Choose an organization belonging to the same provider and click **Link**. Manual associations are marked **Unverified**. **Verify organization** sends a read-only `GET /v1/models` using the inference API key to its fixed first-party origin and checks the organization response header against a connected Console account. It does not send a model inference request or the Console session. If the provider omits that header, verification fails and the explicit unverified mapping remains available.

OpenAI verification honors fixed `OpenAI-Organization` and `OpenAI-Project` credential headers. Dynamic selectors and conflicting authentication overrides cannot be verified. Verification is bound to a fingerprint of the credential and its custom headers; changing those settings makes the UI report it as unverified until verified again, including after a server restart. Fingerprints are not included in management responses. Manual relinking always resets verification.

## Balance meaning and freshness

- A balance is the last observed USD credit balance, not a subscription usage limit. Missing amounts remain unknown; a reported zero remains zero.
- Grant amounts, remaining credits and expiration dates are shown only when provided. Expired grants remain visible. A missing expiration is not treated as unlimited or as an upcoming reset.
- **Refresh balance** fetches provider data on demand. There is no background provider polling in this release. After five minutes, a successful observation becomes stale in both API snapshots and an open UI page.
- Failed requests preserve the last observation and show a safe error. A multi-organization login can expose readable organizations while showing denied organizations as unknown or stale. A successful observation from another connection is retained.
- No fresh balance or usable credits are inferred from an expired session, permission denial, unsupported response, or unknown field.

## Persistence and deployment

Billing state lives in `<directory-containing-config.yaml>/console-billing/`. The directory must be writable by the server and must survive container replacement. If only the configuration file is mounted read-only, mount a separate writable directory at the adjacent `console-billing` path. Home mode disables these management endpoints.

The directory is owner-only (`0700`). `billing.enc` holds the AES-GCM encrypted sessions, snapshots and bindings; `session.key` is an owner-only (`0600`) local encryption key. Keep both together in a restricted backup. Encryption does not protect against someone who can read both files or act as the server user. Losing the key requires new Console logins. Corruption fails closed rather than overwriting the store. Multiple server processes must not actively share this sidecar: writes detect conflicts and require a restart/reload of the stale process before retrying.

Deploy the matching management UI build to obtain `#/billing`. Updating this backend alone does not add that page to an older UI artifact. No production deployment is part of this change.

## Provider compatibility

Console billing endpoints are undocumented and may change independently of supported inference APIs. The implementation uses fixed origins and bounded, read-only requests, refuses redirects, and reports an unsupported endpoint/schema instead of inventing a balance or organization. Fixture tests validate parsing, persistence and management behavior. Live logins and balances have not been validated with real accounts.

Claude adapter evidence is the [CodexBar Console guide](https://github.com/steipete/CodexBar/blob/main/docs/claudeplatform.md) and its referenced provider source. OpenAI's [official API overview](https://developers.openai.com/api/reference/overview) documents organization/project request selection; the legacy dashboard credit shape is corroborated by the [dsh-api-balance implementation](https://github.com/GPIOX/dsh-api-balance/blob/main/lib/index.js). OpenAI `/v1/me` organization discovery is documented for ordinary API keys; compatibility with the browser dashboard session remains unverified. This is not an official public billing-balance API integration.

The v8 HTTP contract is in [management-api-v8.md](management-api-v8.md).
