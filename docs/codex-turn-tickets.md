# Experimental Codex turn-state tickets

`codex.turn-state-ticket` is disabled by default. When enabled, a bounded
background worker obtains `x-codex-turn-state` values from the selected OAuth
accounts. Only HTTP 200 responses with a 292-character `gAAAAA` value are
accepted. The one-hour TTL and ten-minute refresh margin are local policies,
not guarantees about upstream validity or model quality.

An empty harvest proxy uses the account's existing proxy, then the global
proxy. Probes use independent HTTP/1.1 connections, do not refresh OAuth
credentials, and do not update account health or usage statistics. Configure
`account-ids` and `models` to limit a trial. The default models are Astra and
Sol. Probe concurrency defaults to two, with a sixty-second pause between
completed cycles. Acquiring a ticket makes real upstream requests.

Valid tickets are isolated by auth ID, ChatGPT account ID, and model. They are
held only in memory and injected into HTTP requests and new upstream
WebSocket handshakes. Existing WebSocket connections are not reconnected on
refresh. Missing or expired tickets preserve normal forwarding. Account files
and client request bodies are unchanged. Custom upstreams and API keys are
excluded. Disable the configuration to stop injection immediately; the worker
reads the updated configuration at its next cycle.

Authenticated `GET /v0/management/codex-turn-tickets` returns hashed account
identities, probe status/length, expiration and readiness. `headers_prepared`
counts header preparation, not physical WebSocket handshakes. No opaque
ticket, OAuth token or proxy password is returned. Turn-state headers are
redacted in request logs.

Reference design: [sub2api PR #7315](https://github.com/Wei-Shaw/sub2api/pull/7315).
This branch deliberately keeps missing tickets fail-open for controlled
testing. Establish real acquisition and an interleaved same-account/model
control before attributing any error-rate change to this feature.
