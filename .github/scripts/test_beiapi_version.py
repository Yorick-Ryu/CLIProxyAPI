"""Exercise version numbering against isolated Git histories, without services."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("beiapi-version.sh").resolve()


class VersionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name) / "repo"
        self.repo.mkdir()
        self.env = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull,
                        GIT_CONFIG_SYSTEM=os.devnull)
        self.git("init", "-b", "production")
        self.git("config", "user.name", "Version Test")
        self.git("config", "user.email", "version@example.invalid")
        self.commit("upstream")
        self.upstream = self.git("rev-parse", "HEAD")
        self.git("tag", "v8.0.44")
        self.commit("complete selective integration")
        self.anchor = self.git("rev-parse", "HEAD")
        self.metadata = self.repo / ".github/beiapi-version-base"
        self.metadata.parent.mkdir()
        self.write_base()
        self.git("add", ".")
        self.commit("record version baseline")

    def git(self, *args):
        return subprocess.check_output(
            ["git", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.devnull,
             *args], cwd=self.repo, env=self.env, stderr=subprocess.PIPE,
            text=True).strip()

    def commit(self, message):
        self.git("commit", "--allow-empty", "-m", message)

    def write_base(self, version="v8.0.44", upstream=None, anchor=None):
        self.metadata.write_text(
            f"upstream_version={version}\n"
            f"upstream_commit={upstream or self.upstream}\n"
            f"integration_commit={anchor or self.anchor}\n")

    def version(self, cwd=None):
        return subprocess.run(["bash", str(SCRIPT)], cwd=cwd or self.repo,
                              env=self.env, capture_output=True, text=True)

    def assert_version(self, expected):
        result = self.version()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), expected)

    def test_commits_and_rebuilds(self):
        self.assert_version("v8.0.44-beiapi.1")
        self.assert_version("v8.0.44-beiapi.1")
        self.commit("local fix")
        self.assert_version("v8.0.44-beiapi.2")

    def test_dirty_tracked_and_untracked_files(self):
        extra = self.repo / "draft.txt"
        extra.write_text("draft")
        self.assert_version("v8.0.44-beiapi.1-dirty")
        extra.unlink()
        self.metadata.write_text(self.metadata.read_text().replace(
            self.anchor, self.git("rev-parse", "HEAD")))
        self.assert_version("v8.0.44-beiapi.0-dirty")

    def test_branch_merge_counts_once(self):
        self.git("checkout", "-b", "feature")
        self.commit("feature part one")
        self.commit("feature part two")
        self.git("checkout", "production")
        self.git("merge", "--no-ff", "feature", "-m", "merge feature")
        self.assert_version("v8.0.44-beiapi.2")

    def test_selective_integration_and_baseline_reset(self):
        self.git("checkout", "-b", "upstream", self.upstream)
        self.commit("new upstream release")
        new_upstream = self.git("rev-parse", "HEAD")
        self.git("tag", "v8.0.45")
        self.git("checkout", "production")
        # Merely fetching a release must not advance the recorded baseline.
        self.assert_version("v8.0.44-beiapi.1")
        self.commit("selectively integrate approved upstream changes")
        anchor = self.git("rev-parse", "HEAD")
        self.write_base("v8.0.45", new_upstream, anchor)
        self.git("add", ".")
        self.commit("advance reviewed baseline")
        self.assert_version("v8.0.45-beiapi.1")

    def test_side_branch_anchor_rejected(self):
        self.git("checkout", "-b", "feature")
        self.commit("side branch change")
        anchor = self.git("rev-parse", "HEAD")
        self.git("checkout", "production")
        self.git("merge", "--no-ff", "feature", "-m", "merge feature")
        self.write_base(anchor=anchor)
        result = self.version()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("first-parent", result.stderr)

    def test_prerelease_baseline(self):
        self.git("tag", "v1.0.0-rc.46", self.upstream)
        self.write_base("v1.0.0-rc.46")
        self.git("add", ".")
        self.commit("use prerelease baseline")
        self.assert_version("v1.0.0-rc.46.beiapi.2")

    def test_inconsistent_tag_rejected(self):
        self.write_base(upstream=self.anchor)
        result = self.version()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not match", result.stderr)

    def test_shallow_history_rejected(self):
        clone = Path(self.temp.name) / "shallow"
        self.git("clone", "--depth=1", self.repo.as_uri(), str(clone))
        result = self.version(clone)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Full Git history", result.stderr)


if __name__ == "__main__":
    unittest.main()
