# Codex environment timezone

Configure `codex.timezone` in CPA's existing YAML configuration. Omission is
identical to `off`; existing configuration reload handles changes.

```yaml
codex:
  timezone:
    mode: off
    zone: ""
```

- `off`: preserve client environment text, with no GeoIP request.
- `fixed`: set `zone` to an IANA name, for example `America/Los_Angeles`.
- `egress`: infer a timezone using `https://ipwho.is/?fields=success,timezone.id`
  through the execution-scoped proxy, account proxy, global proxy, or inherited
  transport, in that priority order. `zone` is ignored. Only the outgoing IP is
  exposed to this service; no prompts, account tokens, or client headers are sent.

## Independent account policies

Settings belong to the credential's own metadata (auth JSON), alongside its
other account-level settings. No account-ID map is kept in the global YAML.

```json
{
  "codex_timezone_mode": "fixed",
  "codex_timezone": "Asia/Tokyo"
}
```

These are fields to merge into an existing account, not a complete credential.
`codex_timezone_mode` accepts `off`, `fixed`, or `egress`. Missing, null, empty,
or `inherit` selects the global default. `fixed` requires that account's own
`codex_timezone` IANA zone. Account mode overrides the global mode even when
the global mode is `off`.

Use the existing authenticated `PATCH /v0/management/auth-files/fields` endpoint:

```json
{
  "name": "existing-account.json",
  "codex_timezone_mode": "fixed",
  "codex_timezone": "Asia/Tokyo"
}
```

To restore inheritance, submit both timezone fields as null. The management
API validates changes before updating the account and exposes valid explicit
settings in its auth-files account list. Existing metadata persistence and auth
file watching carry these fields into the runtime. No new panel controls are
included; use the existing account JSON/field editor or management API.
HTTP and WebSocket preparation select the currently executing credential's
settings, including retries on another account. Malformed manually edited
settings leave client text unchanged instead of applying the global rewrite.

Only standalone `<environment_context>...</environment_context>` user text
blocks with an existing `<timezone>` tag are changed. Existing `<current_date>`
tags become today's date in the selected zone. Other message content and unknown
JSON fields are preserved. Missing tags are not inserted. HTTP, compact, serial
WebSocket, and duplex WebSocket request preparation share this behavior.

Lookup results are cached for six hours per account/route; failures for one
minute. Cache size is bounded to 1024 entries. Cold lookups wait under the incoming
request context and can add latency; cancellation and lookup errors retain the
original text. Invalid proxy configuration never falls back to a direct lookup.
GeoIP is approximate: rotating exits and destination-based proxy routing can
produce a different OpenAI exit than the lookup observes. Changing the proxy
selects a different cache entry. Nothing changes the host or container timezone.

Fixed mode avoids the lookup dependency. Both rewriting modes can affect date
interpretation and prefix cache reuse, and may disagree with client tools' local
time. Existing historical environment blocks are also rewritten. This feature
makes no claim to improve model quality. To disable all rewriting, set the global
`mode: off` and remove the account overrides (or set every account mode to `off`).
