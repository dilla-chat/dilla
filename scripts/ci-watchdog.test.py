"""Exercise cancellation decisions and the authenticated HTTP boundary without GitHub."""
import importlib.util
from contextlib import redirect_stderr
from functools import partial
import io
import json
import os
import sys
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from urllib.error import HTTPError, URLError

sys.dont_write_bytecode = True
path = Path(__file__).with_name("ci-watchdog.py")
spec = importlib.util.spec_from_file_location("ci_watchdog", path)
watchdog = importlib.util.module_from_spec(spec)
spec.loader.exec_module(watchdog)


def job(name, status="completed", conclusion="success", steps=None):
    return {"id": 1, "name": name, "status": status, "conclusion": conclusion,
            "steps": steps or []}


class Clock:
    def __init__(self):
        self.now = 0
        self.waits = []

    def sleep(self, seconds):
        self.waits.append(seconds)
        self.now += seconds


class Transport:
    def __init__(self, responses):
        self.responses = iter(responses)
        self.requests = []

    def __call__(self, request, timeout):
        self.requests.append((request, timeout))
        response = next(self.responses)
        if isinstance(response, Exception):
            raise response
        return io.BytesIO(json.dumps(response).encode())


class WatchdogTests(unittest.TestCase):
    def client(self, responses, clock=None):
        self.assertTrue(hasattr(watchdog, "GitHubRun"), "the run-scoped watchdog is missing")
        transport = Transport(responses)
        clock = clock or Clock()
        api = watchdog.GitHubRun("owner/repo", "123", "test-token", opener=transport,
                                 sleep=clock.sleep)
        return api, transport, clock

    def watch(self, responses, limit=100):
        api, transport, clock = self.client(responses)
        result = watchdog.watch(api, sleep=clock.sleep, monotonic=lambda: clock.now,
                                max_seconds=limit)
        return result, transport, clock

    def test_completed_failure_cancels_only_its_own_run_immediately(self):
        result, transport, clock = self.watch([
            {"total_count": 2, "jobs": [job("docs", conclusion="failure"), job("go", "in_progress", None)]},
            {},
        ])
        self.assertEqual(result, "cancelled")
        self.assertEqual(clock.waits, [])
        request, timeout = transport.requests[-1]
        self.assertEqual(request.full_url, "https://api.github.com/repos/owner/repo/actions/runs/123/cancel")
        self.assertEqual(request.get_method(), "POST")
        self.assertEqual(request.get_header("Authorization"), "Bearer test-token")
        self.assertLessEqual(timeout, 10)

    def test_failed_step_gives_uploads_three_seconds_then_cancels(self):
        failed = job("desktop / desktop-voice-mac-arm64", "in_progress", None,
                     [{"name": "voice", "status": "completed", "conclusion": "failure"},
                      {"name": "upload trace", "status": "in_progress", "conclusion": None}])
        result, transport, clock = self.watch([{"total_count": 1, "jobs": [failed]}, {}])
        self.assertEqual(result, "cancelled")
        self.assertEqual(clock.waits, [3])
        self.assertEqual(len(transport.requests), 2)

    def test_every_non_success_terminal_job_cancels_including_external_cancellation(self):
        for conclusion in ["failure", "timed_out", "startup_failure", "action_required", "cancelled", "stale"]:
            with self.subTest(conclusion=conclusion):
                result, transport, _ = self.watch([
                    {"total_count": 1, "jobs": [job("native", conclusion=conclusion)]},
                    {},
                ])
                self.assertEqual(result, "cancelled")
                self.assertEqual(transport.requests[-1][0].get_method(), "POST")

    def test_cancel_conflict_is_benign_only_after_confirming_completed_run(self):
        conflict = HTTPError("https://api.github.com", 409, "test-token", {}, io.BytesIO(b"test-token"))
        api, transport, _ = self.client([conflict, {"status": "completed", "conclusion": "cancelled"}])
        api.cancel()
        self.assertEqual([(r.get_method(), r.full_url) for r, _ in transport.requests], [
            ("POST", "https://api.github.com/repos/owner/repo/actions/runs/123/cancel"),
            ("GET", "https://api.github.com/repos/owner/repo/actions/runs/123"),
        ])

    def test_cancel_conflict_on_running_workflow_is_unconfirmed_failure(self):
        conflict = HTTPError("https://api.github.com", 409, "test-token", {}, io.BytesIO(b"test-token"))
        api, transport, _ = self.client([conflict, {"status": "in_progress", "conclusion": None}])
        with self.assertRaisesRegex(RuntimeError, "cancellation unconfirmed") as caught:
            api.cancel()
        self.assertNotIn("test-token", str(caught.exception))
        self.assertEqual(len(transport.requests), 2)

    def run_main(self, responses, *, event=None, env_changes=None, deadline=False):
        transport = Transport(responses)
        environment = {"GITHUB_REPOSITORY": "owner/repo", "GITHUB_RUN_ID": "123", "GH_TOKEN": "test-token",
                       "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/main"}
        environment.update(env_changes or {})
        clock = Clock()
        original_watch = watchdog.watch
        with TemporaryDirectory(prefix="watchdog-test-", dir=path.parent) as directory:
            event_path = Path(directory) / "event.json"
            event_path.write_text(json.dumps(event or {}))
            environment["GITHUB_EVENT_PATH"] = str(event_path)
            stderr = io.StringIO()
            timer = partial(original_watch, sleep=clock.sleep, monotonic=lambda: clock.now,
                            max_seconds=10 if deadline else 100)
            with patch.dict(os.environ, environment, clear=True), \
                    patch.object(watchdog, "build_opener", return_value=SimpleNamespace(open=transport)), \
                    patch.object(watchdog, "watch", timer), redirect_stderr(stderr):
                result = watchdog.main()
        return result, transport, stderr.getvalue()

    def test_terminal_jobs_api_error_attempts_cancel_and_keeps_failure_exit(self):
        error = HTTPError("https://api.github.com", 403, "test-token", {}, io.BytesIO(b"test-token"))
        result, transport, stderr = self.run_main([error, {}])
        self.assertEqual(result, 1)
        self.assertEqual([(r.get_method(), r.full_url) for r, _ in transport.requests], [
            ("GET", "https://api.github.com/repos/owner/repo/actions/runs/123/jobs?filter=latest&per_page=100&page=1"),
            ("POST", "https://api.github.com/repos/owner/repo/actions/runs/123/cancel"),
        ])
        self.assertNotIn("test-token", stderr)

    def test_deadline_attempts_cancel_and_keeps_failure_exit(self):
        result, transport, stderr = self.run_main([
            {"total_count": 1, "jobs": [job("go", "in_progress", None)]},
            {"total_count": 1, "jobs": [job("go", "in_progress", None)]}, {},
        ], deadline=True)
        self.assertEqual(result, 1)
        self.assertEqual(transport.requests[-1][0].get_method(), "POST")
        self.assertEqual(transport.requests[-1][0].full_url,
                         "https://api.github.com/repos/owner/repo/actions/runs/123/cancel")
        self.assertIn("deadline", stderr)

    def test_best_effort_cancel_failure_stays_nonzero_and_redacts_error_bodies(self):
        errors = [HTTPError("https://api.github.com", 403, "test-token", {}, io.BytesIO(b"test-token"))
                  for _ in range(2)]
        result, transport, stderr = self.run_main(errors)
        self.assertEqual(result, 1)
        self.assertEqual(len(transport.requests), 2)
        self.assertEqual(transport.requests[-1][0].get_method(), "POST")
        self.assertIn("cancellation", stderr)
        self.assertNotIn("test-token", stderr)

    def test_invalid_or_untrusted_environment_never_attempts_cancellation(self):
        for changes in [{"GITHUB_RUN_ID": "../456"}, {"GITHUB_EVENT_NAME": "pull_request"},
                        {"GH_TOKEN": ""}, {"GITHUB_REF": "refs/heads/feature"}]:
            with self.subTest(changes=changes):
                result, transport, _ = self.run_main([], env_changes=changes)
                self.assertEqual(result, 1)
                self.assertEqual(transport.requests, [])

    def test_startup_and_finished_roots_do_not_hide_later_matrix_children(self):
        roots = [job("changes"), job("docs")]
        result, transport, clock = self.watch([
            {"total_count": 0, "jobs": []},
            {"total_count": 2, "jobs": roots},
            {"total_count": 3, "jobs": roots + [job("desktop / client", "in_progress", None)]},
            {"total_count": 6, "jobs": roots + [job("desktop / client"), job("ci-work-complete"),
              job("ci-watchdog", "in_progress", None), job("ci-ok", "queued", None)]},
        ])
        self.assertEqual(result, "complete")
        self.assertEqual(clock.waits, [5, 5, 5])
        self.assertTrue(all(r.get_method() == "GET" for r, _ in transport.requests))

    def test_completion_requires_real_roots_and_all_real_jobs(self):
        roots = [job("changes"), job("docs")]
        result, transport, clock = self.watch([
            {"total_count": 1, "jobs": [job("ci-work-complete")]},
            {"total_count": 4, "jobs": roots + [job("ci-work-complete"), job("go", "queued", None)]},
            {"total_count": 4, "jobs": roots + [job("ci-work-complete"), job("go", conclusion="skipped")]},
        ])
        self.assertEqual(result, "complete")
        self.assertEqual(clock.waits, [5, 5])
        self.assertEqual(len(transport.requests), 3)

    def test_failure_on_second_page_is_not_missed(self):
        first = [job("passing") for _ in range(100)]
        result, transport, _ = self.watch([
            {"total_count": 101, "jobs": first},
            {"total_count": 101, "jobs": [job("native", conclusion="timed_out")]}, {},
        ])
        self.assertEqual(result, "cancelled")
        self.assertEqual(transport.requests[0][0].full_url,
                         "https://api.github.com/repos/owner/repo/actions/runs/123/jobs?filter=latest&per_page=100&page=1")
        self.assertTrue(transport.requests[1][0].full_url.endswith("page=2"))

    def test_incomplete_page_cannot_look_like_finished_ci(self):
        api, _, _ = self.client([{"total_count": 2, "jobs": []}])
        with self.assertRaisesRegex(RuntimeError, "incomplete jobs page"):
            api.jobs()

    def test_redirect_refuses_to_forward_the_token(self):
        request = watchdog.Request("https://api.github.com/repos/owner/repo/actions/runs/123/jobs",
                                   headers={"Authorization": "Bearer test-token"})
        with self.assertRaises(HTTPError) as caught:
            watchdog.NoRedirect().redirect_request(request, io.BytesIO(b""), 302,
                                                   "redirect", {}, "https://evil.invalid/steal")
        caught.exception.close()

    def test_transient_http_and_network_errors_have_bounded_retries(self):
        error = HTTPError("https://api.github.com", 503, "unavailable", {}, io.BytesIO(b"secret"))
        api, transport, clock = self.client([error, URLError("offline"), {"total_count": 0, "jobs": []}])
        self.assertEqual(api.jobs(), [])
        self.assertEqual(clock.waits, [1, 2])
        self.assertEqual(len(transport.requests), 3)
        api, transport, _ = self.client([URLError("offline")] * 3)
        with self.assertRaisesRegex(RuntimeError, "unavailable after 3 attempts"):
            api.jobs()
        self.assertEqual(len(transport.requests), 3)

    def test_permission_error_fails_without_retry_or_disclosing_token(self):
        error = HTTPError("https://api.github.com", 403, "test-token", {}, io.BytesIO(b"test-token"))
        api, transport, clock = self.client([error])
        with self.assertRaisesRegex(RuntimeError, "GitHub API returned HTTP 403") as caught:
            api.jobs()
        self.assertNotIn("test-token", str(caught.exception))
        self.assertEqual(clock.waits, [])
        self.assertEqual(len(transport.requests), 1)

    def test_hard_deadline_cannot_poll_forever(self):
        api, transport, clock = self.client([{"total_count": 0, "jobs": []}] * 2)
        with self.assertRaisesRegex(RuntimeError, "deadline"):
            watchdog.watch(api, sleep=clock.sleep, monotonic=lambda: clock.now, max_seconds=10)
        self.assertEqual(clock.now, 10)
        self.assertEqual(len(transport.requests), 2)

    def test_untrusted_events_and_arbitrary_run_paths_are_rejected_before_http(self):
        self.assertTrue(hasattr(watchdog, "GitHubRun"), "the run-scoped watchdog is missing")
        for repo, run in [("owner/repo/other", "123"), ("owner/repo", "../456"),
                          ("https://evil.invalid", "123"), ("owner/repo", "0")]:
            with self.assertRaises(ValueError):
                watchdog.GitHubRun(repo, run, "token")
        base = {"GITHUB_REPOSITORY": "owner/repo", "GITHUB_RUN_ID": "123", "GH_TOKEN": "token",
                "GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/pull/1/merge"}
        trusted = {"pull_request": {"head": {"repo": {"full_name": "owner/repo"}}}}
        self.assertIsInstance(watchdog.from_environment(base, trusted), watchdog.GitHubRun)
        for event, changes in [({"pull_request": {"head": {"repo": {"full_name": "fork/repo"}}}}, {}),
                               ({}, {}), ({}, {"GITHUB_EVENT_NAME": "workflow_dispatch"}),
                               ({}, {"GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/feature"})]:
            with self.assertRaises(ValueError):
                watchdog.from_environment(base | changes, event)
        self.assertIsInstance(watchdog.from_environment(base | {"GITHUB_EVENT_NAME": "push",
                              "GITHUB_REF": "refs/heads/main"}, {}), watchdog.GitHubRun)


if __name__ == "__main__":
    unittest.main()
