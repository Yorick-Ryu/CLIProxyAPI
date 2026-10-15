# Codex environment dates and timezones

```yaml
oauth:
  providers:
    codex:
      timezone:
        mode: off
```

- `off` (default): preserve the client's environment date and timezone.
- `local`: generate both fields using the CPA process environment's timezone
  and the current clock. `TZ` takes priority over the operating system settings;
  an unresolvable IANA timezone falls back to `Etc/UTC` for both fields.
- `egress`: resolve the actual account/proxy exit timezone, then generate its
  current date. Lookup failures leave the original environment unchanged.

Generation updates only the latest standalone user `<environment_context>` block
present in the request. Missing date/timezone fields in that block are added;
earlier historical blocks, quoted content, instructions, and tool output are
preserved. No environment message is created when the request has none.
Both fields use one clock reading. Unix timestamps in turn metadata keep their
original meaning and are not rewritten. Explicit payload rules run afterward.

Accounts may override the global mode with `codex_timezone_mode`. Omit it or use
`inherit` to inherit the global setting.

The former `fixed` mode, `timezone.zone`, and account `codex_timezone` setting are
retired. Before deploying this version with an old `fixed` configuration, choose
`off`, `local`, or `egress` and remove the obsolete zone setting. `local` uses the
CPA host/container environment, not the original client's environment.
