# Codex request location

CPA can override the request location that Codex sends upstream. One resolved
location feeds both the time-context timezone and the Web Search
`user_location`.

## Global setting

```yaml
oauth:
  providers:
    codex:
      request-location:
        enabled: false
        country: CN
        region: Shanghai
        city: Shanghai
        timezone: Asia/Shanghai
```

The global location is disabled by default. When `enabled` is true, every field
is required: `country` is a two-letter uppercase ISO code, `region` and `city`
are 1-128 characters without control characters, and `timezone` is a valid
IANA name. An invalid enabled location stops configuration loading. Reload
reports changes as `codex.request-location: updated`.

## Account setting

Account settings live in the credential's auth JSON metadata:

```json
{
  "codex_request_location_mode": "on",
  "codex_request_location": {
    "country": "US",
    "region": "Ohio",
    "city": "Piketon",
    "timezone": "America/New_York"
  }
}
```

`codex_request_location_mode` resolves as follows:

| Mode | Behavior |
| --- | --- |
| `inherit` (default) | Use the global location only when it is enabled. |
| `off` | Never rewrite this account's requests, even if a location is stored. |
| `on` | Use the account's `codex_request_location`, otherwise the global location. |
| `egress` | Resolve the selected route's exit location through GeoIP. |

Put a proxy-specific location in `codex_request_location` on the accounts that
use that proxy, and set their mode to `on`. A stored location is used only in
`on` mode. Missing, null, or empty mode means `inherit`.

Use the authenticated `PATCH /v8/management/credentials/fields` endpoint:

```json
{
  "name": "existing-account.json",
  "codex_request_location_mode": "on",
  "codex_request_location": {"country": "US", "region": "Ohio", "city": "Piketon", "timezone": "America/New_York"}
}
```

The endpoint validates both fields and rejects nested field paths. Set both to
null to restore inheritance. The auth-files account list shows the effective
mode and any stored location. A malformed manually edited value leaves requests
unchanged.

The former `codex.timezone.mode`, `codex_timezone_mode`, `codex_timezone`, and
`local` mode are removed without compatibility handling.

## Request rewrites

Only standalone `input_text` blocks are changed: user `<environment_context>`
and developer `<codex_apps_client_time_context>`. When
`internal_chat_message_metadata_passthrough.content_item_kinds` is present, the
matching kind is required, so explicit ordinary text is left unchanged. The
whole XML document must be valid, and only `<timezone>` elements directly under
the root are replaced. `current_date` keeps the client's value, missing fields
are not added, and quoted content, nested examples, and tool output are
preserved.

Top-level `tools[]` entries with `type: web_search` or a `web_search_` prefix
receive a complete replacement `user_location` with `type: approximate`,
`country`, `region`, `city`, and `timezone`. Function tools and nested tools are
unchanged, and no search tool is added. HTTP, compact, and WebSocket preparation
share this behavior. Payload rules run afterward.

## Egress lookup

To use automatic exit location for an account, apply:

```json
{
  "name": "existing-account.json",
  "codex_request_location_mode": "egress",
  "codex_request_location": null
}
```

Set this mode on each Codex account that should use automatic lookup, including
new accounts added later. The global `request-location.enabled` switch controls
the fixed location inherited by accounts; it does not enable egress lookup.
It can remain false when every intended account explicitly uses `egress`.
Existing proxy routing is preserved; this setting uses its exit location and
does not assign or change the outgoing IP.

`egress` queries `https://ipwho.is/?fields=success,country_code,region,city,timezone.id`
through the execution-scoped proxy, account proxy, global proxy, or inherited
transport. Only the outgoing IP is exposed; prompts, tokens, and client headers
are not sent. Results are cached per account and route for six hours, and
failures for one minute. Canceled lookups are not cached; callers still waiting
retry through a new shared lookup using an active caller's context.
Invalid proxy configuration never falls back to a direct lookup.
A failed lookup preserves the request. An incomplete location
can still update the timezone but leaves Web Search unchanged. GeoIP is
approximate, and rotating exits can differ from the lookup.

Overrides change only request fields. They do not change the host clock, Unix
timestamps, or local tools such as `date`, which still report the client's real
time. Overriding the timezone can affect the model's sense of local time.

Reference: `zyycn/codex-proxy-rs` revision
`35e8b73075e3bd068629a848f650f24d6cdd153a`,
`backend/crates/providers/openai/src/transport/request.rs`.
