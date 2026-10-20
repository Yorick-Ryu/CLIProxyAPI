# Account operations

Authenticated CPA v8 management routes at `/v8/management/account-ops` proxy a
single local worker over an authenticated Unix socket. OMCPA uses a handwritten
response projection and its existing authenticated management facade. Browsers do
not receive CPA keys, saved passwords, TOTP secrets, OAuth URLs or tokens.

Build the CPA core with `CGO_ENABLED=1` on Unix, as in its Dockerfile. Native
dynamic plugin loading is unavailable in a `CGO_ENABLED=0` binary. Verify both
the plugin inventory and its authenticated state route before a rollout.

## Run and verify

Install this package with Python 3.12 or newer and install the existing login
project's dependencies in a dedicated virtual environment. The login project is
an explicit deployment input; do not copy its dotenv files, account pools or
runtime directories. Supply a verified CloakBrowser executable and Xvfb on Linux.

Use `deploy/config.example.json` and `deploy/cpa-account-ops.service` as the DMIT
layout. Create an independent random 32-byte `master.key`, a random service token
and a private copy of the target CPA management key. Secret files must be regular
files with mode 0600. The service account owns its encrypted SQLite state and job
directories. Back up SQLite with `Store.backup` and preserve its master key as a
pair. This key is independent of the OMCPA master key.

Mount `/run/cpa-account-ops` into the CPA container at the same path, and mount only
`service.key` read-only at `/run/cpa-account-ops-service.key`. Set
`CPA_ACCOUNT_OPS_SOCKET=/run/cpa-account-ops/ops.sock` and
`CPA_ACCOUNT_OPS_TOKEN_FILE=/run/cpa-account-ops-service.key`. Neither the worker
nor OMCPA receives a Docker socket. The example worker origin is the host loopback
management port; the container loopback is a different namespace.

Run tests with `PYTHONPATH=tools/account-ops python -m unittest discover -s
tools/account-ops/tests`. In the server workspace use the login project's Python
virtual environment. Go tests cover v8 authentication, response boundaries, OAuth
workspace/configuration guards and atomic credential commits.

## Public protocol

- `GET /capabilities`, `/accounts`, `/monitor`, `/schedules` report actual instance
  capabilities. A missing browser executor rejects browser submissions before
  queueing. DMIT defaults to preserving proxies, disables automatic enrollment and
  rejects automatic schedule takeover.
- `POST /imports/preview` creates a one-hour encrypted draft and returns only email,
  order, fixed-point amount, absolute Beijing expiry and validation issues.
- `POST /jobs` requires `Idempotency-Key`. Kinds are `record`, `login`, `reauth` and
  `apply_preferences`. A draft with no `row_index` creates a batch with child jobs;
  ambiguous per-account amounts require `confirm_amount: true`.
- `GET /jobs`, `/jobs/:id`, `/jobs/:id/events` and `/records` support durable task
  details and cursor pagination. `POST /jobs/:id/cancel` requests cancellation;
  a live browser must acknowledge exit before releasing its slot. `resume` checks
  the existing owned browser; it never starts another OAuth flow.
- `POST /accounts/:id/credentials` is write-only. `GET/PUT /preferences/:profile`
  use version comparisons. Reauthorization retains the exact workspace and account
  settings. New jobs freeze preferences and credential versions.
- Core-only `/targets` and `/targets/:id/preferences` use a configuration hash and
  guarded writes under the credential file lock. OAuth final commits also check
  the worker lease, cancellation and original workspace. These routes are not
  exposed through OMCPA. Existing v0 routes remain unchanged.

## Execution and recovery

One browser runs globally; email locks also span workspace aliases. Each job runs
in its own subprocess and process group. No login-module patches are shared across
accounts. Password/MFA/workspace/callback phases recheck policy and cancellation.
Expired leases fail closed. Linux child ownership records include boot ID, PID,
process group and kernel start time. A dead leader with surviving unknown group
members remains blocked for operator reconciliation. Cancellation never signals an
unidentified process, and service shutdown stops its own cgroup only.

New-account priority increases require one fresh `gpt-6.1-sol / low` candy result.
A failed, skipped or unavailable result cannot raise priority. An explicit
`quality_skip: user_explicit` is recorded as a user override. Periodic scans retain
the separate three-result downgrade/restore policy and 240-second restoration
wait. Cooling is never patched by default. New-login order expiry is written in
Beijing format; reauthorization does not replace the existing note.

Clouvider legacy sources are read as data, with bounded log tails and explicit
incomplete-history reporting. Its old cron entries remain authoritative until
separate ownership receipts and observed removal permit takeover. Ordinary retry
failures are limited to two attempts, at least 15 minutes after the previous
failure. Human challenges and cancelled recoveries require manual action.

An optional fixed `proxy_helper` executable receives only instance, target ID,
credential filename and configuration hash. It must implement privileged allocation,
all-config/history validation, existing allocation locks and verified readback.
The worker has no implicit privileged allocator. Without it the capability is
false; DMIT preserves its existing proxy. `policy.ipv6_candidates` defines the
never-used / 48-hour / 24-hour ordering and rejects unknown history.

An optional fixed `notification_helper` receives an opaque notification ID, job
ID, instance and enumerated reason. Return `{"sent":true}` only after delivery.
Outbox retries are independent of login, with stable IDs and exponential backoff;
an absent helper leaves notifications pending. No Feishu integration is included.

Private drafts expire after one hour; frozen job inputs remain until one hour
after completion. Structured events have 90-day retention, subject to unresolved
notification references. Browser logs and ownership files are private operational
evidence removed one hour after a terminal result; unresolved jobs retain them
for ownership reconciliation. Never put
secret files, logs or database backups into source archives or public artifacts.
