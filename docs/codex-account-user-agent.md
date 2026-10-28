# Persisted Codex account User-Agents

UA assignment, stable-release discovery and rolling upgrades are owned by the
account operations plugin. CPA no longer implements `account-user-agent` or
`version-sync`; remove these obsolete settings during migration.

CPA applies credential `headers.User-Agent`, `headers.Version` and
`headers.Originator` to OAuth HTTP requests and new WebSocket handshakes,
including with cloaking enabled. Existing connections are not forcibly closed.
Account headers override global defaults; API-key and SIWC behavior is unchanged.
Final administrator/model header rules retain their existing precedence.

Before upgrading an installation that used runtime-generated identities, use the
plugin migration tool with that installation's `.codex-version-history` cache to
persist existing identities. Do not generate a new five-version window for this
step. Keep migration results and the previous image/configuration for rollback.
See the account operations worker README for the migration command and validation.

Validation: `TestCodexPersistedAccountHeadersAcrossTransports` and
`TestCodexDefaultLinuxTUIIdentity` in `internal/runtime/executor`.
