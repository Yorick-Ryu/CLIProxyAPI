# Rolling Codex account User-Agents

Enable this opt-in mode at service startup:

```yaml
oauth:
  providers:
    codex:
      header-defaults:
        account-user-agent:
          enabled: true
```

`account-user-agent` and `version-sync` are independent sibling settings under
`header-defaults`. The former assigns account-specific Linux UAs and version
slots; the latter synchronizes all accounts to a single latest version. Account
UA mode needs only its own `enabled` flag and takes precedence when both are
enabled. The previous unreleased `version-sync.per-account` layout is removed.

The updater checks official `openai/codex` numeric `rust-vX.Y.Z` tags once at
service startup, including deployment startup. It does not poll while running;
hot reloads and inference requests do not trigger another check. Each candidate
must have a published, non-draft,
non-prerelease GitHub release. The history contains the latest five stable
versions in increasing numeric order. No lookup runs on inference requests.
Discovery uses the configured proxy and unauthenticated GitHub API; rate limits,
network failures and incomplete results retain the previous window.

Each stable account identity deterministically selects one of five age-ranked
slots, a Linux system/version and a terminal. The account identity, rather than
tokens or the caller's API key, seeds this assignment. Accounts may share a
profile. Renaming a credential file or refreshing its token does not change the
profile when account/user identity metadata is retained; credentials without
that metadata fall back to their auth ID. The ordered profile-v1 tables and
terminal eligibility rules must remain stable after their first release.

System names and versions are stored as complete pairs: Ubuntu 20.4.0, 22.4.0,
24.4.0 and 26.4.0; Debian 12.0.0 and 13.0.0; Arch Linux Rolling Release. These
are curated UA templates, not claims that every combination was captured.
Version text is preserved rather than coerced to numeric semver. Ubuntu
26.4.0 appears in the generated UA examples reported in
[Codex #19284](https://github.com/openai/codex/issues/19284#issuecomment-4978082196).

Terminal name/version pairs include `xterm-256color`, `kitty`, `Alacritty`,
`gnome-terminal`, `Ghostty`, `WezTerm`, `Konsole` and `vscode`, using the official
[terminal formatting rules](https://github.com/openai/codex/blob/rust-v0.164.0/codex-rs/terminal-detection/src/lib.rs).
An absent terminal version stays absent; it is not copied from the Codex version
or made up. The additional `ghostty/1.3.1-arch2` pair is restricted to Arch Linux,
matching the app-server response reported in
[Codex #23572](https://github.com/openai/codex/issues/23572).
VS Code's integrated terminal is a terminal token here; the client remains
`codex-tui`, not the `codex_vscode` extension identity.

For example, `[A,B,C,D,E]` becoming `[B,C,D,E,F]` advances each account by one
release, including accounts previously on E. Missing several releases advances
the same slot to the new five-release window. An unchanged or older newest
version cannot roll the window backward. System, architecture (`x86_64`) and
terminal stay fixed. The generated UA uses `codex-tui` and puts the assigned
Codex version in both UA positions and the `Version` header.

The cached version list lives at `<auth-dir>/.codex-version-history`. It contains
no account IDs or credentials and uses a non-JSON filename so the auth watcher
does not ingest it. Updates replace the file atomically before publishing the
new window. A failed cache write retains the previous runtime window. Restarts
can use this cache while offline. With no cache and no complete lookup yet,
existing configured headers remain in effect. An SDK configuration without an
auth directory uses an in-memory window only.

This mode replaces global and credential UA/Version values, as legacy version
sync already replaces configured versions. Explicit credential `Originator`
and final model header overrides retain precedence. Disabled cloaking, API-key
and SIWC requests retain their previous behavior. HTTP/SSE and new WebSocket
handshakes share the profile; an existing WebSocket retains its handshake
headers until it reconnects. No connections are forcibly closed by an update.

This feature changes headers, not TLS. System/version and terminal values come
from fixed Linux tables, not arbitrary combinations. Published Codex versions
are not automatically considered TLS-verified; the configured HTTP and WS
transport profiles remain separate.

The updater mode is selected at startup. Hot reload can stop applying the mode
but does not create a new updater. Enabling it after a disabled/legacy startup
requires a separately authorized service restart. Turning `account-user-agent.enabled` off or
using an older binary restores legacy behavior; account credentials are never
modified and the unused cache can remain in place.

Validation entry points: `TestCodexHistory*`, `TestCodexAccountUserAgents*` in
`internal/runtime/executor/helps`, and `TestCodexAccountHistoryIdentity*` in
`internal/runtime/executor`.
