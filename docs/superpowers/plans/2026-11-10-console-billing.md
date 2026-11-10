# Claude and OpenAI Console Integration Implementation Plan

> **For agentic workers:** Use superpowers:subagent-driven-development for the independent provider/store, management integration, and UI tasks. Follow test-driven-development and review the integrated result.

**Goal:** Connect multiple independent Claude and OpenAI Console sessions, display organization balances and expiring grants, and persist verified or explicitly unverified API-credential mappings.

**Architecture:** Separate Console login connections, provider organization accounts, and API-credential bindings. Store sessions outside inference credentials in an encrypted, owner-only sidecar next to the configuration; never serialize secrets into management responses. Management uses only /v8/management. Tier routing, dollar-reserve enforcement, and earliest-expiry selection are subsequent work; expose accurate data and stable account IDs now without altering routing.

**Tech stack:** Go 1.26 backend, existing React/TypeScript/Bun UI, local uv/Playwright Python login helper.

**Spec:** The accepted design in this conversation: independent login sessions for at least three accounts, optional one-login/many-organization discovery, organization-ID based key association, reconnect without losing accounts/bindings, balances and grant expirations. API credentials retain inference behavior when billing authentication expires.

## Global constraints
- No production deployment, live account login, inference probes, pushes, real credentials, or v0 changes.
- Fixed provider origins, reject redirect-based credential leakage, bounded reads, sanitized errors, preserve unknown versus zero and stale versus fresh data.
- No network under the store mutex; refreshed sessions/deleted connections cannot be overwritten by stale responses. Atomic owner-only encrypted persistence; corruption fails closed without replacing data.
- Existing priority, strategy, affinity and quota protection are unchanged.
- Respect repository instructions and preserve unrelated edits. Work on codex/console-billing branches in the clean existing checkouts; managed worktree creation did not return and was abandoned before any worktree existed.

## Interfaces
Package internal/consolebilling owns these public types and methods:
- Provider string values claude/openai.
- Connection: id, provider, label, status (ready/reconnect_required/error), account_ids, error (safe optional), updated_at.
- Account: id, provider, organization_id, name, connection_id, balance (*float64 USD; null unknown), currency, grants ([]Grant), observed_at (time optional), status (ready/unknown/stale/reconnect_required), error (safe optional).
- Grant: id, name, kind, amount (*float64), remaining (*float64), expires_at (*time.Time). No fabricated remaining amount or expiration if absent upstream.
- Binding: credential_id, account_id, verified (bool). Verification reset on manual rebinding.
- Snapshot: connections ([]Connection), accounts ([]Account), bindings ([]Binding).
- NewManager(dir string, client *http.Client) (*Manager,error); List() Snapshot; Connect(ctx,provider,label,session string) (Connection,error); Reconnect(ctx,id,session string) (Connection,error); Refresh(ctx,id string) error; DeleteConnection(id string) error; Bind(credentialID,accountID string,verified bool) error; Unbind(credentialID string) error.
- JSON arrays always arrays, not null. Explicit JSON field names snake_case.
- Errors expose only safe codes/messages, not provider response text, URLs with tokens, or credentials.

Management routes:
- GET /billing => Snapshot fields plus credentials []{id,label,provider,organization_id?,verification_status?}; credentials are eligible first-party API keys only, never secrets.
- POST /billing/connections {provider,label,session} => {connection}; validate session/discovery before saving.
- PATCH /billing/connections/:id {session} => {connection}; must retain the original connection ID and provider and require organization continuity.
- DELETE /billing/connections/:id => {status:"ok"}; disconnect removes login access but preserves organization account identity, prior data as stale, and mappings.
- POST /billing/connections/:id/refresh => {status:"ok"}; on failure preserve previous data as stale, never treat as zero.
- PUT /billing/bindings {credential_id,account_id} => {status:"ok"}; same provider required; initially unverified.
- DELETE /billing/bindings?credential_id=... => {status:"ok"}.
- POST /billing/credentials/verify {credential_id} => {binding}; use only fixed first-party GET /v1/models with that credential, map organization header to a discovered account; no inference or arbitrary upstream URLs.
- GET /billing/login-helper => Python source for downloadable local helper.

## Review focus
- Cross-provider or cross-organization binding/reconnect mismatch never silently relabels balances.
- Concurrent refresh vs reconnect/delete cannot resurrect credentials or publish wrong-account data.
- Session secrets absent from GET, errors, logs, config dumps and fixtures; redirects cannot forward them.
- Missing/expired grants and malformed financial amounts are not reported as fresh zero or replenishing quota.
- UI connection switching/logout discards stale async responses and never persists Console secrets in browser storage.

### Task 1: Provider adapters and secure account store
**Owner:** provider worker; only internal/consolebilling/**.
- [x] Write failing fixture-based tests for Claude organizations/credits (minor units and tranches), OpenAI dashboard session discovery/credit grants, unknown balances, zero balances, expired grants, 401/403/429/malformed data and redirect rejection. Verify RED.
- [x] Implement adapters using actual public source evidence for undocumented endpoints; no arbitrary endpoint configuration. Record source references in package docs or comments.
- [x] Write RED tests for encrypted persistence/restart, three independent sessions, shared org deduplication, manual mapping validation, reconnect mismatch, failure preserving snapshots, and stale refresh races.
- [x] Implement Manager and secure sidecar. Run package and race tests; provide commit-ready changes without committing while root coordinates.

### Task 2: v8 management endpoints and local login helper
**Owner:** root; handlers/management/console_billing*, handlers/management/assets/console-login.py, server_management_v8.go, docs/console-billing.md and plan.
- [x] Write failing handler tests for secret-free CRUD, eligible API-only credential listing, provider validation, exact organization-header verification, missing accounts and unsafe URLs. Verify RED.
- [x] Add lazy persistent Manager under <config-dir>/console-billing using fixed-origin network requests and existing proxy-aware transport where suitable. Add routes only in v8.
- [x] Add local Playwright helper: three separate isolated login sessions; Claude sessionKey import and OpenAI observed dashboard Bearer session import; management key from getpass/env only, never CLI args or browser; HTTPS uploads except loopback HTTP. New and reconnect supported. Test parsing/capture/upload validation without live login.
- [x] Run handler/route tests, full Go tests and required server build. Update operator/API documentation including endpoint uncertainty and stale-data behavior.

### Task 3: Management UI
**Owner:** UI worker; separate UI repository, feature billing, API module, route/navigation, relevant locales, tests.
- [x] Read backend contract above and nearby conventions. Write RED tests for normalization, secret-free rendering, connection/account/grouping states, and grant expiry handling.
- [x] Add Billing navigation/page: accounts with balances, grants/expiry and freshness; connections with connect/reconnect/refresh/disconnect; API credential linking/unlinking and verification; download helper and copy login command; manual session import fallback with password field, no persistence.
- [x] Respect all themes, accessibility and supported locales (AGENTS requires four; update any additional active locale if relevant). No hardcoded English product text. Preserve single HTML deployment and stale async guards.
- [x] Run Bun verify and browser verification using fixtures; no production credentials. Report evidence without claiming live provider validation.

### Task 4: Integrated review and commits
- [x] Independently review each deliverable and the combined contracts, including all Review Focus conditions; fix important findings with regression tests.
- [x] Verify backend full suite/race targets/build, UI verify, helper tests and a fixture-backed real-browser integration run.
- [x] Commit only request files in sensible groups in both repositories; do not push. Report hashes and limitations. Leave live provider login to the user after implementation.


## Verification record

- Provider and management tests were first observed failing before implementation; focused suites and the full Go suite subsequently pass.
- `go test ./...`, provider/management `go test -race`, `go vet`, and the required `go build -o test-output ./cmd/server` pass. Build output removed.
- Offline helper tests: eight pass, including isolated browser contexts, asynchronous OpenAI capture during terminal input, cleanup, fixed-origin parsing and no-redirect upload.
- UI full verification passes tests, lint, TypeScript and the single-file build. Real Chromium fixture checks cover desktop/mobile, connection creation, helper download, mapping/verification/unlinking, mismatch reconnect, stale refresh errors and browser-storage secrecy.
- Independent review findings addressed: safe actionable errors, cross-connection account publication ordering, partial organization permissions, five-minute client/server freshness, fixed OpenAI organization/project selectors, persisted verification fingerprints, and disconnecting expired sessions.
- Live provider logins have not been performed. OpenAI dashboard-session organization discovery is explicitly unverified. No production deployment, routing changes, push or merge is included.

The completed changes are kept on local `codex/console-billing` branches in both repositories, committed in separate provider/store, management/helper, documentation, UI and locale groups.
