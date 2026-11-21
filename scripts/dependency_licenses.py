# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

import base64
import csv
import hashlib
import io
import json
import os
import re
import subprocess
import sys
import tempfile
import tomllib
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path


NOTICES = "THIRD_PARTY_NOTICES"
INVENTORY = "docs/license-inventory.json"
CSV = "third_party/licenses.csv"
MANIFESTS = ("go.mod", "go.sum", "cmd/gsxsimconnect/Cargo.toml", "cmd/gsxsimconnect/Cargo.lock")
FILES = (NOTICES, INVENTORY, CSV)
NOTICE_FILE = re.compile(r"^(?:licen[cs]e|unlicen[cs]e|copying|notice|patents|third.party.notices)(?:$|[._-])", re.I)
PRIMARY_LICENSE = re.compile(r"^(?:licen[cs]e|unlicen[cs]e|copying)(?:$|[._-])", re.I)
SPDX = re.compile(r"\(*[A-Za-z0-9][A-Za-z0-9.()+ /-]*")
CRATES_IO = "registry+https://github.com/rust-lang/crates.io-index"
MARKER = "<!-- gsxcomms-dependency-licenses -->"


def run(*args, cwd=None, data=None):
    return subprocess.run(args, cwd=cwd, input=data, stdout=subprocess.PIPE, check=True).stdout


def json_stream(raw):
    decoder = json.JSONDecoder()
    rest = raw.decode("utf-8").lstrip()
    while rest:
        value, end = decoder.raw_decode(rest)
        yield value
        rest = rest[end:].lstrip()


def notice_files(directory):
    return sorted(p.name for p in directory.iterdir() if p.is_file() and NOTICE_FILE.match(p.name))


def read_notice(directory, name):
    file = (directory / name).resolve()
    if Path(name).is_absolute() or not file.is_relative_to(directory.resolve()) or not file.is_file():
        raise ValueError(f"Missing or invalid notice file {name} in {directory.name}")
    return file.read_text(encoding="utf-8-sig")


def classify(files, work):
    # Use an explicit SPDX expression when upstream publishes one in its license text
    declared = {
        match.strip()
        for file in files
        for match in re.findall(r"^\s*SPDX-License-Identifier:\s*(.+)$", file.read_text(encoding="utf-8-sig"), re.M)
    }
    if len(declared) == 1 and SPDX.fullmatch(next(iter(declared))):
        return next(iter(declared))
    report = work / "classification.json"
    run("identify_license", "-json", str(report), *files, cwd=work)
    results = json.loads(report.read_text(encoding="utf-8"))
    licenses = []
    for result in results:
        matches = {
            m["Name"] for m in result["Classifications"]
            if m["Name"] != "Copyright" and m["Confidence"] >= 0.90
        }
        licenses.append(matches)
    if not licenses or len(licenses) != len(files) or any(
        len(matches) != 1 or matches != licenses[0] for matches in licenses
    ):
        raise ValueError("License text could not be classified unambiguously")
    name = next(iter(licenses[0]))
    if not SPDX.fullmatch(name):
        raise ValueError("License classifier did not return an SPDX identifier")
    return name


@dataclass(frozen=True)
class Dependency:
    ecosystem: str
    name: str
    version: str
    license: str
    texts: tuple[str, ...]

    @property
    def digest(self):
        return hashlib.sha256(json.dumps(self.texts, ensure_ascii=False).encode("utf-8")).hexdigest()

    @property
    def label(self):
        return f"`{self.name} {self.version}` (`{self.license}`)"


def scan(inputs, inventory):
    deps = []
    updated = {}
    with tempfile.TemporaryDirectory(prefix="gsx-license-") as tmp:
        work = Path(tmp)
        for name, contents in inputs.items():
            path = work / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(contents)

        # Resolve the whole Go module graph without loading or compiling application code
        modules = list(json_stream(run("go", "list", "-m", "-mod=readonly", "-json", "all", cwd=work)))
        if any(module.get("Replace") for module in modules):
            raise ValueError("Go module replacements require manual license review")
        refs = [f'{module["Path"]}@{module["Version"]}' for module in modules if not module.get("Main")]
        if refs:
            downloaded = json_stream(run("go", "mod", "download", "-json", *refs, cwd=work))
            for module in downloaded:
                name, version, directory = module["Path"], module["Version"], Path(module["Dir"])
                previous = inventory["modules"].get(name, {})
                files = notice_files(directory)
                # Keep reviewed paths outside the module root, such as the go-gui asset license
                files += [file for file in previous.get("files", []) if "/" in file]
                primary = [directory / file for file in files if PRIMARY_LICENSE.match(file)]
                if not primary:
                    raise ValueError(f"No Go license file for {name} {version}")
                try:
                    license = previous["license"] if previous.get("version") == version else classify(primary, work)
                except ValueError as error:
                    raise ValueError(f"{name} {version}: {error}") from error
                choice = previous.get("license", "")
                if choice.endswith(f"(chosen from {license})"):
                    license = choice
                updated[name] = {"version": version, "license": license, "files": files}
                texts = [read_notice(directory, file) for file in files]
                if name == "github.com/alecthomas/chroma/v2":
                    # The Liberation Mono font license lives in the Go source header
                    font = "formatters/svg/font_liberation_mono.go"
                    source = read_notice(directory, font)
                    header, found, _ = source.partition("\npackage ")
                    if not found or "SIL OPEN FONT LICENSE" not in header:
                        raise ValueError(f"Review the embedded Liberation Mono notice in {name} {version}")
                    texts.append(f"// {font}\n\n" + re.sub(r"^// ?", "", header, flags=re.M).strip() + "\n")
                deps.append(Dependency("Go", name, version, license, tuple(texts)))
        if sum(dep.ecosystem == "Go" for dep in deps) != len(refs):
            raise ValueError("Go returned an incomplete module inventory")

        # Use Cargo's locked registry artifacts and include target-specific crates
        manifest = work / MANIFESTS[2]
        lock = tomllib.loads((work / MANIFESTS[3]).read_text(encoding="utf-8"))
        expected = {(p["name"], p["version"]): p.get("source") for p in lock["package"]}
        if any(source not in (None, CRATES_IO) for source in expected.values()):
            raise ValueError("Non-crates.io dependencies require manual license review")
        (manifest.parent / "src").mkdir()
        (manifest.parent / "src/main.rs").touch()
        rust = json.loads(run("cargo", "+1.98.0", "metadata", "--locked", "--all-features",
                              "--format-version", "1", "--manifest-path", manifest, cwd=work))
        if {(p["name"], p["version"]): p["source"] for p in rust["packages"]} != expected:
            raise ValueError("Cargo metadata does not cover every package in Cargo.lock")
        for crate in rust["packages"]:
            if crate["source"] is None:
                continue
            name, version, directory = crate["name"], crate["version"], Path(crate["manifest_path"]).parent
            files = notice_files(directory)
            declared_file = crate["license_file"]
            if declared_file and declared_file not in files:
                files.append(declared_file)
            texts = tuple(read_notice(directory, file) for file in files)
            license = crate["license"]
            if not texts and version == "0.2.1" and license == "MIT" and name in (
                "flybywireless-simconnect", "flybywireless-simconnect-derive", "flybywireless-simconnect-proto"
            ):
                texts = ("See FlyByWireless-LICENSE above.\n",)
            if not texts:
                raise ValueError(f"Missing Rust license text for {name} {version}")
            if not license and declared_file:
                try:
                    license = classify([directory / declared_file], work)
                except ValueError as error:
                    raise ValueError(f"{name} {version}: {error}") from error
            if not license or not SPDX.fullmatch(license):
                raise ValueError(f"Review the Rust license for {name} {version}")
            deps.append(Dependency("Rust", name, version, license, texts))
    return sorted(deps, key=lambda dep: (dep.ecosystem, dep.name, dep.version)), updated


def warnings(before, after):
    groups = defaultdict(lambda: ([], []))
    for index, deps in enumerate((before, after)):
        for dep in deps:
            groups[dep.ecosystem, dep.name][index].append(dep)
    changes = []
    for (ecosystem, _), (old, new) in sorted(groups.items()):
        removed = [dep for dep in old if dep not in new]
        added = [dep for dep in new if dep not in old]
        if len(removed) == len(added) == 1:
            first, second = removed[0], added[0]
            if first.license != second.license:
                changes.append(f"- {ecosystem} license changed from {first.label} to {second.label}.")
            elif first.digest != second.digest:
                changes.append(f"- {ecosystem} license or notice text changed for {second.label}.")
        else:
            changes.extend(f"- {ecosystem} dependency added: {dep.label}." for dep in added)
            changes.extend(f"- {ecosystem} dependency removed: {dep.label}." for dep in removed)
    return changes


def render(notices, inventory, deps, modules):
    # Migrate open Dependabot branches created before the notice markers were added
    if "=== BEGIN GO DEPENDENCIES ===\n" not in notices:
        go_start = notices.index("========== github.com/")
        go_end = notices.index("========== Go standard library and runtime ==========")
        rust_start = re.search(r"^========== [\w-]+ [\d][^\n]* \(", notices[go_end:], re.M)
        if not rust_start:
            raise ValueError("Could not locate the original Rust dependency notices")
        rust_start = go_end + rust_start.start()
        notices = (
            notices[:go_start] + "=== BEGIN GO DEPENDENCIES ===\n=== END GO DEPENDENCIES ===\n\n"
            + notices[go_end:rust_start] + "=== BEGIN RUST DEPENDENCIES ===\n=== END RUST DEPENDENCIES ===\n"
        )
    # Replace only the generated Go and Rust sections
    for ecosystem in ("Go", "Rust"):
        begin = f"=== BEGIN {ecosystem.upper()} DEPENDENCIES ===\n"
        end = f"=== END {ecosystem.upper()} DEPENDENCIES ==="
        if notices.count(begin) != 1 or notices.count(end) != 1:
            raise ValueError(f"Missing or duplicate {ecosystem} dependency notice markers")
        prefix, body = notices.split(begin)
        _, suffix = body.split(end)
        section = "\n".join(
            f"========== {dep.name} {dep.version} ({dep.license}) ==========\n"
            + "".join(text if text.endswith("\n") else text + "\n" for text in dep.texts)
            for dep in deps if dep.ecosystem == ecosystem
        )
        notices = prefix + begin + section + end + suffix

    report = io.StringIO()
    writer = csv.writer(report, lineterminator="\n")
    writer.writerow(("Ecosystem", "Dependency", "Version", "License", "Notice SHA256"))
    writer.writerows((dep.ecosystem, dep.name, dep.version, dep.license, dep.digest) for dep in deps)
    inventory["modules"] = modules
    return {
        NOTICES: notices.encode("utf-8"),
        INVENTORY: (json.dumps(inventory, ensure_ascii=False, indent=2) + "\n").encode("utf-8"),
        CSV: report.getvalue().encode("utf-8"),
    }


def git_file(revision, path):
    return run("git", "show", f"{revision}:{path}")


def audit(head, output):
    base = run("git", "merge-base", "HEAD", head).decode().strip()
    inventory = json.loads(git_file(base, INVENTORY))
    before, _ = scan({name: git_file(base, name) for name in MANIFESTS}, inventory)
    after, modules = scan({name: git_file(head, name) for name in MANIFESTS}, inventory)
    changes = warnings(before, after)
    summary = "\n".join(changes) if changes else "No Go or Rust license changes detected."
    with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a", encoding="utf-8") as file:
        file.write(f"## Dependency licenses\n\n{summary}\n")
    output.mkdir(parents=True, exist_ok=True)
    result = render(git_file(head, NOTICES).decode("utf-8"), inventory, after, modules)
    additions = []
    for name, contents in result.items():
        present = run("git", "ls-tree", "--name-only", head, "--", name).strip()
        if not present or contents != git_file(head, name):
            additions.append({"path": name, "contents": base64.b64encode(contents).decode("ascii")})
    (output / "result.json").write_text(json.dumps({"changes": changes, "additions": additions}), encoding="utf-8")


def api(endpoint, method="GET", data=None, *flags):
    args = ["gh", "api", endpoint, "--method", method, *flags]
    if data is not None:
        args += ["--input", "-"]
    return json.loads(run(*args, data=None if data is None else json.dumps(data).encode("utf-8")))


def eligible_pr(pr, repo):
    return (
        pr["state"] == "open"
        and pr["user"]["login"] == "dependabot[bot]"
        and pr["base"]["ref"] == "main"
        and pr["head"]["ref"].startswith("dependabot/")
        and (pr["head"]["repo"] or {}).get("full_name") == repo
    )


def target(number):
    repo = os.environ["GITHUB_REPOSITORY"]
    pr = api(f"repos/{repo}/pulls/{number}")
    if not eligible_pr(pr, repo):
        raise ValueError("Select an open Dependabot PR from this repository targeting main")
    return pr["head"]["sha"]


def comment(endpoint, number, head, report, warn):
    comments = f"{endpoint}/issues/{number}/comments"
    existing = next((
        entry for page in api(comments, "GET", None, "--paginate", "--slurp") for entry in page
        if entry["user"]["login"] == "github-actions[bot]" and entry["body"].startswith(MARKER)
    ), None)
    if not warn and not existing:
        return
    repo = os.environ["GITHUB_REPOSITORY"]
    run_url = f'{os.environ["GITHUB_SERVER_URL"]}/{repo}/actions/runs/{os.environ["GITHUB_RUN_ID"]}'
    body = {"body": f"{MARKER}\n{report}\n\n[License check for `{head[:12]}`]({run_url})"}
    if existing:
        api(f"{endpoint}/issues/comments/{existing['id']}", "PATCH", body)
    else:
        api(comments, "POST", body)


def commit_notices(repo, head, additions):
    if any(file["path"] not in FILES for file in additions):
        raise ValueError("Unexpected path in generated license update")
    commit = api("graphql", "POST", {
        "query": "mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input: $input) { commit { oid } } }",
        "variables": {"input": {
            "branch": {"repositoryNameWithOwner": repo, "branchName": head["ref"]},
            "expectedHeadOid": head["sha"],
            "message": {"headline": "chore: update dependency license notices [skip dependabot]"},
            "fileChanges": {"additions": additions},
        }},
    })
    print("Updated Dependabot branch at", commit["data"]["createCommitOnBranch"]["commit"]["oid"])


def publish(output, status):
    repo = os.environ["GITHUB_REPOSITORY"]
    number = int(os.environ["PR_NUMBER"])
    expected_head = os.environ["PR_HEAD"]
    if not expected_head:
        return
    endpoint = f"repos/{repo}"
    pr = api(f"{endpoint}/pulls/{number}")
    head = pr["head"]
    if not eligible_pr(pr, repo) or head["sha"] != expected_head:
        return

    if status != "success":
        comment(endpoint, number, head["sha"], "**License check failed.** Review the workflow run before merging.", True)
        return

    result = json.loads((output / "result.json").read_text(encoding="utf-8"))
    changes = result["changes"]
    report = ("**Review these dependency license changes before merging.**\n\n" + "\n".join(changes)
              if changes else "No Go or Rust license changes detected.")
    try:
        comment(endpoint, number, head["sha"], report, bool(changes))
    finally:
        # A comment permission failure must not prevent a valid notice commit
        if result["additions"]:
            commit_notices(repo, head, result["additions"])


if __name__ == "__main__":
    if sys.argv[1] == "target":
        print(target(int(sys.argv[2])))
    elif sys.argv[1] == "audit":
        audit(sys.argv[2], Path(sys.argv[3]))
    elif sys.argv[1] == "publish":
        publish(Path(sys.argv[2]), sys.argv[3])
