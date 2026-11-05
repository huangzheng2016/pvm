#!/usr/bin/env python3
"""Reuse only this workflow's successful push/master run at the exact commit."""
import json
import os
import urllib.error
import urllib.parse
import urllib.request

RAW_ARTIFACTS = {"raw-pvm_linux_amd64", "raw-pvm_linux_arm64", "raw-pvm_darwin_arm64"}


def trusted_run(run, repository, sha, workflow_id):
    return (run.get("event") == "push" and run.get("head_branch") == "master"
            and run.get("head_sha") == sha and run.get("conclusion") == "success"
            and run.get("status") == "completed" and run.get("workflow_id") == workflow_id
            and run.get("repository", {}).get("full_name") == repository
            and run.get("head_repository", {}).get("full_name") == repository)


def complete_artifacts(artifacts):
    available = {a["name"] for a in artifacts if not a.get("expired", True) and a.get("size_in_bytes", 0) > 0}
    return RAW_ARTIFACTS.issubset(available)


def reusable_run(api, repository, sha):
    workflow_id = api("actions/workflows/build.yml")["id"]
    query = urllib.parse.urlencode(dict(event="push", branch="master", status="success", head_sha=sha, per_page=100))
    for run in api(f"actions/workflows/{workflow_id}/runs?{query}")["workflow_runs"]:
        if trusted_run(run, repository, sha, workflow_id):
            artifacts = api(f'actions/runs/{run["id"]}/artifacts?per_page=100')["artifacts"]
            if complete_artifacts(artifacts):
                return str(run["id"])
    return ""


def main():
    run_id = ""
    if os.environ["GITHUB_REF"].startswith("refs/tags/"):
        repository = os.environ["GITHUB_REPOSITORY"]
        def api(path):
            request = urllib.request.Request(f"https://api.github.com/repos/{repository}/{path}", headers={"Authorization": "Bearer " + os.environ["GH_TOKEN"], "Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"})
            with urllib.request.urlopen(request, timeout=30) as response:
                return json.load(response)
        try:
            run_id = reusable_run(api, repository, os.environ["GITHUB_SHA"])
        except (urllib.error.URLError, KeyError, ValueError):
            print("Reuse lookup unavailable; running all release gates.")
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        output.write(f"run_id={run_id}\n")
    print("Using verified master run " + run_id if run_id else "Running native and network validation.")


if __name__ == "__main__":
    main()
