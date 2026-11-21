# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import dependency_licenses as licenses


class LicenseTest(unittest.TestCase):
    def test_license_and_notice_changes(self):
        old_go = licenses.Dependency("Go", "example.com/lib", "v1.0.0", "MIT", ("Original terms",))
        new_go = licenses.Dependency("Go", "example.com/lib", "v1.1.0", "MIT", ("Updated terms",))
        old_rust = licenses.Dependency("Rust", "demo", "1.0.0", "MIT OR Apache-2.0", ("Terms",))
        new_rust = licenses.Dependency("Rust", "demo", "2.0.0", "Apache-2.0", ("Terms",))
        other_rust = licenses.Dependency("Rust", "demo", "0.1.0", "BSD-3-Clause", ("Other terms",))
        self.assertEqual(licenses.warnings([old_go, old_rust, other_rust], [new_go, new_rust, other_rust]), [
            "- Go license or notice text changed for `example.com/lib v1.1.0` (`MIT`).",
            "- Rust license changed from `demo 1.0.0` (`MIT OR Apache-2.0`) to `demo 2.0.0` (`Apache-2.0`).",
        ])
        version_only = licenses.Dependency("Go", "example.com/lib", "v1.2.0", "MIT", new_go.texts)
        self.assertEqual(licenses.warnings([new_go], [version_only]), [])

    def test_render_preserves_manual_notices(self):
        original = ("Manual intro\n=== BEGIN GO DEPENDENCIES ===\nold license\n=== END GO DEPENDENCIES ===\n"
                    "Manual asset notice\n=== BEGIN RUST DEPENDENCIES ===\n=== END RUST DEPENDENCIES ===\n")
        dep = licenses.Dependency("Go", "example.com/lib", "v1.1.0", "ISC", ("New license",))
        files = licenses.render(original, {}, [dep], {})
        self.assertIn(b"Manual intro\n", files[licenses.NOTICES])
        self.assertIn(b"Manual asset notice\n", files[licenses.NOTICES])
        self.assertIn(b"example.com/lib v1.1.0 (ISC) ==========\nNew license\n", files[licenses.NOTICES])
        self.assertIn(b"Go,example.com/lib,v1.1.0,ISC,", files[licenses.CSV])
        self.assertEqual(files, licenses.render(files[licenses.NOTICES].decode(), {}, [dep], {}))

    def test_publish_requires_current_head_and_keeps_commit_when_comment_fails(self):
        repo = "owner/project"
        head = {"sha": "a" * 40, "ref": "dependabot/cargo/demo", "repo": {"full_name": repo}}
        pr = {"state": "open", "user": {"login": "dependabot[bot]"}, "base": {"ref": "main"}, "head": head}
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            additions = [{"path": licenses.CSV, "contents": "YQo="}]
            (path / "result.json").write_text(json.dumps({"changes": ["Changed"], "additions": additions}), encoding="utf-8")
            with (
                patch.dict(os.environ, {"GITHUB_REPOSITORY": repo, "PR_NUMBER": "10", "PR_HEAD": head["sha"]}),
                patch.object(licenses, "api", return_value=pr) as api,
                patch.object(licenses, "comment", side_effect=subprocess.CalledProcessError(1, "gh")),
            ):
                pr["head"] = {**head, "sha": "b" * 40}
                licenses.publish(path, "success")
                self.assertEqual(api.call_count, 1)
                pr["head"] = head
                with patch.object(licenses, "commit_notices") as commit:
                    with self.assertRaises(subprocess.CalledProcessError):
                        licenses.publish(path, "success")
                    commit.assert_called_once_with(repo, head, additions)

    def test_manual_target_rejects_non_dependabot_pr(self):
        repo = "owner/project"
        pr = {
            "state": "open", "user": {"login": "dependabot[bot]"}, "base": {"ref": "main"},
            "head": {"sha": "a" * 40, "ref": "dependabot/cargo/demo", "repo": {"full_name": repo}},
        }
        with patch.dict(os.environ, {"GITHUB_REPOSITORY": repo}), patch.object(licenses, "api", return_value=pr):
            self.assertEqual(licenses.target(10), pr["head"]["sha"])
            pr["user"]["login"] = "somebody-else"
            with self.assertRaises(ValueError):
                licenses.target(10)


if __name__ == "__main__":
    unittest.main()
