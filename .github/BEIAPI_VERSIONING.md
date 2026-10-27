# BeiAPI build versions

Production builds use `v<upstream>-beiapi.<revision>`, for example
`v8.0.44-beiapi.1`. This is the CPA application version, independent of the
Codex client `Version` and `User-Agent` headers.

`.github/beiapi-version-base` records the reviewed upstream release, its exact
commit, and the local commit that completed that integration. Selective upstream
integration is supported: the version identifies the reviewed source baseline,
not a claim that every upstream patch is enabled. Preserve exclusions and local
adaptations in the integration commit messages and acceptance records.

The revision counts first-parent commits after the local integration commit.
The integration itself is revision zero. Each subsequent commit, including docs,
reverts and merge commits, increments it once; commits inside a merged branch do
not inflate it. Rebuilding the same commit does not change the revision. Local
uncommitted or untracked files append `-dirty`.

After completing a new upstream integration, update all three fields together
in a follow-up commit. That commit becomes revision one for the new baseline.
Do not advance the baseline just because a new upstream tag exists. Full history
and the recorded upstream tag are required; missing or inconsistent metadata
fails the build instead of guessing from `git describe`.

Run `bash .github/scripts/beiapi-version.sh` from the checkout to inspect the
version. The production image workflow and local Docker build helpers use it
as the binary's `VERSION` build argument. The PowerShell helper requires Git
Bash. Direct `go build` and Docker builds without build arguments retain `dev`.
Upstream tag-release workflows exclude BeiAPI tags to avoid duplicate builds.

Ordinary branch pushes do not build a production image. Commit and push the
source to `beiapi-production`, run the version script in a clean checkout, then
create and push exactly that version tag. Only `v*-beiapi.*` tag pushes trigger
the production image workflow. It verifies that the tag equals the calculated
version and its commit belongs to `origin/beiapi-production` before building.
The workflow fetches only the recorded upstream tag from the upstream repository
and verifies its commit against the pinned metadata.

Production images retain branch and immutable `sha-<commit>` tags and also
publish the fork version tag and OCI version label. Pin deployments by digest
or SHA; a source commit or image build does not deploy or restart a service.
