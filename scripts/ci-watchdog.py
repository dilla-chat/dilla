#!/usr/bin/env python3
"""Cancel only this trusted CI run at its first failure, using its job-local token."""
import json
import os
import re
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.request import HTTPRedirectHandler, Request, build_opener


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Never forward the bearer token to an API-supplied redirect destination.
        raise HTTPError(req.full_url, code, "redirect refused", headers, fp)


class GitHubRun:
    def __init__(self, repository, run_id, token, *, opener=None, sleep=time.sleep):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("invalid GITHUB_REPOSITORY")
        if not re.fullmatch(r"[1-9][0-9]*", run_id):
            raise ValueError("invalid GITHUB_RUN_ID")
        if not token:
            raise ValueError("missing job token")
        self.url = f"https://api.github.com/repos/{repository}/actions/runs/{run_id}"
        self.token = token
        self.open = opener or build_opener(NoRedirect()).open
        self.sleep = sleep

    def request(self, suffix, method="GET"):
        for attempt in range(3):
            request = Request(self.url + suffix, method=method, headers={
                "Accept": "application/vnd.github+json",
                "Authorization": f"Bearer {self.token}",
                "X-GitHub-Api-Version": "2026-03-10",
            })
            try:
                with self.open(request, timeout=8) as response:
                    body = response.read()
                    return json.loads(body) if body else {}
            except HTTPError as error:
                code = error.code
                error.close()
                if method == "POST" and code == 409:
                    # A stuck running workflow can also return 409. Only a fresh
                    # state read proves that external completion won this race.
                    state = self.request("")
                    if state.get("status") == "completed":
                        return {}
                    raise RuntimeError("GitHub cancellation unconfirmed: HTTP 409 and run is not completed") from None
                if code not in (429, 500, 502, 503, 504):
                    raise RuntimeError(f"GitHub API returned HTTP {code}") from None
            except (URLError, TimeoutError, OSError):
                pass
            if attempt < 2:
                self.sleep(attempt + 1)
        raise RuntimeError("GitHub API unavailable after 3 attempts")

    def jobs(self):
        jobs = []
        for page in range(1, 101):
            data = self.request(f"/jobs?filter=latest&per_page=100&page={page}")
            batch = data["jobs"]
            jobs.extend(batch)
            if len(jobs) >= data["total_count"]:
                return jobs
            if not batch:
                raise RuntimeError("GitHub returned an incomplete jobs page")
        raise RuntimeError("GitHub job pagination exceeded its bound")

    def cancel(self):
        self.request("/cancel", "POST")


def from_environment(env, event):
    repository = env.get("GITHUB_REPOSITORY", "")
    event_name = env.get("GITHUB_EVENT_NAME")
    same_repo_pr = (event_name == "pull_request" and
                    event.get("pull_request", {}).get("head", {}).get("repo", {}).get("full_name") == repository)
    main_push = event_name == "push" and env.get("GITHUB_REF") == "refs/heads/main"
    if not (same_repo_pr or main_push):
        raise ValueError("watchdog requires a same-repository pull request or main push")
    return GitHubRun(repository, env.get("GITHUB_RUN_ID", ""), env.get("GH_TOKEN", ""))


def watch(api, *, sleep=time.sleep, monotonic=time.monotonic, max_seconds=89 * 60):
    deadline = monotonic() + max_seconds
    acceptable = {"success", "skipped", "neutral"}
    while monotonic() < deadline:
        # ci-ok waits for us. The completion barrier waits for every real DAG leaf,
        # including reusable-workflow children that GitHub discovers after startup.
        jobs = [j for j in api.jobs() if j["name"] not in {"ci-watchdog", "ci-ok"}]
        for job in jobs:
            completed_failure = (job["status"] == "completed" and
                                 job.get("conclusion") not in acceptable)
            step_failure = any(s["status"] == "completed" and s.get("conclusion") not in acceptable
                               for s in job.get("steps", []))
            if completed_failure or step_failure:
                print("CI failure detected; cancelling this workflow run.", flush=True)
                # Completed jobs have finished failure-artifact uploads. A failed
                # step gets only three seconds for uploads, never an unbounded wait.
                if not completed_failure:
                    sleep(min(3, max(0, deadline - monotonic())))
                api.cancel()
                return "cancelled"
        names = {j["name"] for j in jobs if j["status"] == "completed"}
        if {"changes", "docs", "ci-work-complete"} <= names and all(j["status"] == "completed" for j in jobs):
            print("All CI work completed; watcher finished.", flush=True)
            return "complete"
        sleep(min(5, max(0, deadline - monotonic())))
    raise RuntimeError("CI watchdog deadline exceeded")


def main():
    api = None
    try:
        with open(os.environ["GITHUB_EVENT_PATH"], encoding="utf-8") as event_file:
            event = json.load(event_file)
        api = from_environment(os.environ, event)
        watch(api)
    except Exception as error:
        # Requests sanitize network failures; unexpected exceptions reveal only
        # their type. Never dump headers, API bodies or the bearer token.
        detail = str(error) if isinstance(error, (RuntimeError, ValueError, KeyError)) else type(error).__name__
        token = os.environ.get("GH_TOKEN")
        if token:
            detail = detail.replace(token, "***")
        print(f"::error::CI watchdog failed: {detail}", file=sys.stderr)
        # Only a fully validated trusted client may act. Losing monitoring must
        # abort the expensive jobs too, while keeping this watcher visibly red.
        if api is not None:
            try:
                api.cancel()
            except Exception:
                print("::error::CI watchdog could not confirm workflow cancellation", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
