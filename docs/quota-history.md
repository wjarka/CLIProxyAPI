# Weekly quota history

Weekly quota collection runs in the proxy, without an open dashboard. It supports
Claude, Codex, Antigravity, Devin, Kimi, Meta, xAI and configured Z.AI Coding Plan
keys. Only provider-reported weekly windows are saved; monthly and session-only
plans do not acquire synthetic weekly limits. Each reported model pool is separate.

Configure it in the v8 configuration:

```yaml
observability:
  quota-history:
    enabled: true
    path: quota-history.json
    retention-weeks: 52
```

The defaults are enabled and 52 weeks. Relative paths resolve beside the loaded
configuration file. The archive must be outside the auth directory to avoid the
credential watcher. Mount its directory on durable storage in a container. Path
and retention changes take effect after restart; disabling collection is respected
on configuration reload. When the service starts with history disabled, enabling
it requires a restart. A handler without a configuration path does not write to
the current working directory.

The archive uses an atomic replacement, private file permissions and a size limit.
It contains only opaque account identities, display labels, current credential
joins and normalized quota readings. Tokens and provider response bodies are never
saved. A corrupt archive is preserved rather than overwritten; move it aside to
start a new archive. Storage failure does not prevent inference or proxy startup;
the API reports unavailable/degraded storage and may expose memory-only readings.

Collection normally runs every 15 minutes with jitter, increasing to once a minute
in the final ten minutes before a known reset. Refresh requests share in-flight
work, enforce a one-minute minimum interval and honor provider backoff/Retry-After.
Claude uses the existing shared usage cache and keeps its original measurement
timestamp. All requests are read-only quota requests through management transports;
collection never runs inference or consumes a manual reset grant.

Authenticated v8 Management API endpoints:

- `GET /v8/management/credentials/quota-history?from=<RFC3339>&to=<RFC3339>`
  reads saved data without contacting providers. Ranges are positive and at most
  52 weeks. The default covers the previous and following seven days.
- `POST /v8/management/credentials/quota-history/refresh` with
  `{"credential_ids":[]}` schedules eligible accounts; an empty list selects all.

The timeline displays the last observed unused percentage, not an exact reading
at reset. A reading more than 15 minutes before reset is marked stale. Unknown
percentages remain distinct from zero. Changed reset boundaries are uncertain;
old readings are never relabelled onto a replacement cycle. A historical start
is shown only when explicitly reported or established by a previous reset.
Deleted credentials retain archived history. A replacement account starts a new
identity unless continuity is supported by stable account claims. History cannot
be reconstructed for weeks before collection began.
