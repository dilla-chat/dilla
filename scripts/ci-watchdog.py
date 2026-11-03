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


def failure_state(job):
    acceptable = {"success", "skipped", "neutral"}
    completed_failure = (job["status"] == "completed" and
                         job.get("conclusion") not in acceptable)
    step_failure = any(s["status"] == "completed" and s.get("conclusion") not in acceptable
                       for s in job.get("steps", []))
    return completed_failure, step_failure


class GitHubRun:
    def __init__(self, repository, run_id, token, *, opener=None, sleep=time.sleep, monotonic=time.monotonic):
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
        self.monotonic = monotonic

    def request(self, suffix, method="GET", *, deadline=None):
        # Only jobs() supplies a recovery deadline. Cancellation and other reads
        # retain their original three-attempt policy.
        attempts = 32 if deadline is not None else 3
        last_reason = "not-observed"
        for attempt in range(attempts):
            remaining = deadline - self.monotonic() if deadline is not None else 8
            if remaining <= 0:
                break
            retry_after = 0
            request = Request(self.url + suffix, method=method, headers={
                "Accept": "application/vnd.github+json",
                "Authorization": f"Bearer {self.token}",
                "X-GitHub-Api-Version": "2026-03-10",
            })
            try:
                with self.open(request, timeout=min(8, remaining)) as response:
                    body = response.read()
                    return json.loads(body) if body else {}
            except (json.JSONDecodeError, UnicodeDecodeError):
                raise RuntimeError("GitHub API returned malformed JSON") from None
            except HTTPError as error:
                code = error.code
                header = (error.headers.get("Retry-After", "") if error.headers else "").strip()
                if re.fullmatch(r"[0-9]{1,10}", header):
                    retry_after = int(header)
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
                last_reason = f"HTTP {code}"
                if code == 429 and not retry_after:
                    retry_after = 60
            except URLError as error:
                last_reason = "timeout" if isinstance(error.reason, TimeoutError) else "network"
            except TimeoutError:
                last_reason = "timeout"
            except OSError:
                last_reason = "network"
            if attempt + 1 < attempts:
                delay = attempt + 1 if attempt < 2 else 5
                if deadline is not None:
                    delay = max(delay, retry_after)
                    delay = min(delay, max(0, deadline - self.monotonic()))
                    if attempt == 2:
                        print(f"CI jobs read temporarily unavailable (last reason: {last_reason}); bounded recovery continues.", flush=True)
                self.sleep(delay)
        if deadline is not None:
            raise RuntimeError(f"GitHub jobs read recovery budget exhausted (last reason: {last_reason})")
        raise RuntimeError(f"GitHub API unavailable after 3 attempts (last reason: {last_reason})")

    def jobs(self, *, deadline=None):
        # Include socket timeouts, retry waits and all pages in one finite read
        # budget, also bounded by the watcher's existing overall deadline.
        read_deadline = self.monotonic() + 120
        if deadline is not None:
            read_deadline = min(read_deadline, deadline)
        jobs = []
        for page in range(1, 101):
            data = self.request(f"/jobs?filter=latest&per_page=100&page={page}", deadline=read_deadline)
            if not isinstance(data, dict) or not isinstance(data.get("jobs"), list) or type(data.get("total_count")) is not int or data["total_count"] < 0:
                raise RuntimeError("GitHub API returned a malformed jobs response")
            batch = data["jobs"]
            jobs.extend(batch)
            # Never let an unread page or its recovery budget delay a failure
            # already observed. Partial results are returned only for failure;
            # completion still requires every page below.
            if any(j["name"] not in {"ci-watchdog", "ci-ok"} and any(failure_state(j)) for j in batch):
                return jobs
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
    while monotonic() < deadline:
        # ci-ok waits for us. The completion barrier waits for every real DAG leaf,
        # including reusable-workflow children that GitHub discovers after startup.
        jobs = [j for j in api.jobs(deadline=deadline) if j["name"] not in {"ci-watchdog", "ci-ok"}]
        for job in jobs:
            completed_failure, step_failure = failure_state(job)
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
