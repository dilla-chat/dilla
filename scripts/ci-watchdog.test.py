"""Exercise cancellation decisions and the authenticated HTTP boundary without GitHub."""
import importlib.util
from contextlib import redirect_stderr, redirect_stdout
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


def job(name, status="completed", conclusion="success", steps=None, runner_name="runner-1"):
    return {"id": 1, "name": name, "status": status, "conclusion": conclusion,
            "runner_name": runner_name, "steps": steps or []}


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
        if callable(response):
            response = response(request, timeout)
        if isinstance(response, Exception):
            raise response
        return io.BytesIO(json.dumps(response).encode())


class WatchdogTests(unittest.TestCase):
    def client(self, responses, clock=None):
        self.assertTrue(hasattr(watchdog, "GitHubRun"), "the run-scoped watchdog is missing")
        transport = Transport(responses)
        clock = clock or Clock()
        api = watchdog.GitHubRun("owner/repo", "123", "test-token", opener=transport,
                                 sleep=clock.sleep, monotonic=lambda: clock.now)
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
        failed = job("desktop / build-mac-arm64", "in_progress", None,
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
                    patch.object(watchdog, "GitHubRun", partial(watchdog.GitHubRun, sleep=clock.sleep, monotonic=lambda: clock.now)), \
                    patch.object(watchdog, "watch", timer), redirect_stderr(stderr):
                result = watchdog.main()
        return result, transport, stderr.getvalue()

    def test_terminal_jobs_api_error_fails_open_without_cancelling(self):
        error = HTTPError("https://api.github.com", 403, "test-token", {}, io.BytesIO(b"test-token"))
        result, transport, stderr = self.run_main([error])
        self.assertEqual(result, 0)
        self.assertEqual([r.get_method() for r, _ in transport.requests], ["GET"])
        self.assertIn("::warning::CI watchdog stopped watching", stderr)
        self.assertNotIn("test-token", stderr)

    def test_deadline_fails_open_without_cancelling(self):
        result, transport, stderr = self.run_main([
            {"total_count": 1, "jobs": [job("go", "in_progress", None)]},
            {"total_count": 1, "jobs": [job("go", "in_progress", None)]},
        ], deadline=True)
        self.assertEqual(result, 0)
        self.assertTrue(all(r.get_method() == "GET" for r, _ in transport.requests))
        self.assertIn("stopped watching", stderr)
        self.assertIn("deadline", stderr)

    def test_invalid_or_untrusted_environment_never_attempts_cancellation(self):
        for changes in [{"GITHUB_RUN_ID": "../456"}, {"GITHUB_EVENT_NAME": "pull_request"},
                        {"GH_TOKEN": ""}, {"GITHUB_REF": "refs/heads/feature"}]:
            with self.subTest(changes=changes):
                result, transport, _ = self.run_main([], env_changes=changes)
                self.assertEqual(result, 0)
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

    def test_a_rerun_ignores_the_previous_attempts_records_until_its_own_jobs_report(self):
        # filter=latest returns a job's attempt-1 record until the rerun creates its attempt-2 job.
        roots = [job("changes"), job("docs"), job("ci-work-complete")]
        stale = dict(job("desktop / voice", conclusion="cancelled"), run_attempt=1)
        api, transport, clock = self.client([
            {"total_count": 4, "jobs": roots + [stale]},
            {"total_count": 4, "jobs": roots + [dict(job("desktop / voice"), run_attempt=2)]},
        ])
        api.run_attempt = 2
        result = watchdog.watch(api, sleep=clock.sleep, monotonic=lambda: clock.now, max_seconds=100)
        self.assertEqual(result, "complete")
        self.assertTrue(all(r.get_method() == "GET" for r, _ in transport.requests))

    def test_a_rerun_still_cancels_on_its_own_attempts_failure(self):
        roots = [job("changes"), job("docs")]
        api, transport, clock = self.client([
            {"total_count": 3, "jobs": roots + [dict(job("go", conclusion="failure"), run_attempt=2)]},
            {},
        ])
        api.run_attempt = 2
        result = watchdog.watch(api, sleep=clock.sleep, monotonic=lambda: clock.now, max_seconds=100)
        self.assertEqual(result, "cancelled")

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

    def test_first_page_failure_cancels_before_any_second_page_read_or_recovery(self):
        for failed in [job("native", conclusion="failure"),
                       job("native", "in_progress", None, [{"status": "completed", "conclusion": "failure"}])]:
            with self.subTest(status=failed["status"]):
                first = [failed] + [job("passing") for _ in range(99)]
                next_reads = 0
                def second(request, _timeout):
                    nonlocal next_reads
                    if request.get_method() == "GET":
                        next_reads += 1
                        if next_reads <= 3:
                            return URLError("PRIVATE_SECOND_PAGE_OUTAGE")
                        return {"total_count": 101, "jobs": [job("passing")]}
                    return {}
                result, transport, clock = self.watch([
                    {"total_count": 101, "jobs": first}, second, second, second, second, second])
                self.assertEqual(result, "cancelled")
                self.assertEqual(clock.waits, [] if failed["status"] == "completed" else [3])
                self.assertEqual(len(transport.requests), 2)
                self.assertEqual(transport.requests[-1][0].get_method(), "POST")

    def test_first_page_completed_roots_and_ignored_failure_cannot_hide_second_page_work(self):
        first = [job("changes"), job("docs"), job("ci-work-complete"), job("ci-watchdog", conclusion="failure")]
        first += [job("passing") for _ in range(96)]
        result, transport, clock = self.watch([
            {"total_count": 101, "jobs": first},
            {"total_count": 101, "jobs": [job("native", "in_progress", None)]},
            {"total_count": 101, "jobs": first},
            {"total_count": 101, "jobs": [job("native")]},
        ])
        self.assertEqual(result, "complete")
        self.assertEqual(clock.waits, [5])
        self.assertEqual(len(transport.requests), 4)
        self.assertEqual([r.full_url.rsplit("=", 1)[1] for r, _ in transport.requests], ["1", "2", "1", "2"])

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
            api.request("/jobs?filter=latest&per_page=100&page=1")
        self.assertEqual(len(transport.requests), 3)

    def test_jobs_recovers_after_three_transient_failures_then_cancels_observed_failure_without_delay(self):
        for failure in [URLError("PRIVATE_NETWORK_SECRET"), TimeoutError("PRIVATE_TIMEOUT_SECRET"),
                        HTTPError("https://api.github.com", 503, "PRIVATE_HTTP_SECRET", {}, io.BytesIO(b"PRIVATE_BODY_SECRET"))]:
            with self.subTest(failure=type(failure).__name__):
                output = io.StringIO()
                with redirect_stdout(output):
                    result, transport, clock = self.watch([failure] * 3 + [
                        {"total_count": 1, "jobs": [job("native", conclusion="failure")]}, {}])
                self.assertEqual(result, "cancelled")
                self.assertEqual(clock.waits, [1, 2, 5])
                self.assertEqual(len(transport.requests), 5)
                self.assertEqual(transport.requests[-1][0].get_method(), "POST")
                self.assertNotIn("PRIVATE_", output.getvalue())

    def test_job_read_outage_budget_includes_request_time_and_caps_remaining_timeout(self):
        clock = Clock()
        requests = []
        def offline(request, timeout):
            requests.append((request, timeout))
            clock.now += timeout
            raise TimeoutError("PRIVATE_TIMEOUT_SECRET")
        api = watchdog.GitHubRun("owner/repo", "123", "test-token", opener=offline,
                                 sleep=clock.sleep, monotonic=lambda: clock.now)
        with self.assertRaisesRegex(RuntimeError, "recovery budget.*timeout") as caught:
            api.jobs()
        self.assertLessEqual(clock.now, watchdog.JOBS_READ_BUDGET)
        self.assertGreater(clock.now, watchdog.JOBS_READ_BUDGET - watchdog.JOBS_REQUEST_TIMEOUT)
        self.assertTrue(all(0 < timeout <= watchdog.JOBS_REQUEST_TIMEOUT for _, timeout in requests))
        self.assertNotIn("PRIVATE_", str(caught.exception))

    def test_a_jobs_read_cut_off_mid_body_is_retried(self):
        # Main run 41917570494: GitHub cut a jobs response off; IncompleteRead crashed the watcher, which cancelled the run.
        from http.client import IncompleteRead
        clock = Clock()
        calls = []
        class CutOff:
            def __enter__(self):
                return self
            def __exit__(self, *_):
                return False
            def read(self):
                raise IncompleteRead(b'{"total_count": 1, "jo')
        def opener(request, timeout):
            calls.append(request)
            if len(calls) == 1:
                return CutOff()
            return io.BytesIO(json.dumps({"total_count": 0, "jobs": []}).encode())
        api = watchdog.GitHubRun("owner/repo", "123", "test-token", opener=opener,
                                 sleep=clock.sleep, monotonic=lambda: clock.now)
        self.assertEqual(api.jobs(), [])
        self.assertEqual(len(calls), 2)

    def test_a_jobs_read_slower_than_eight_seconds_still_succeeds(self):
        # PR #149's main run: GitHub answered the jobs read in ~9 s every time; an 8 s socket timeout cancelled the run.
        clock = Clock()
        def slow(request, timeout):
            if timeout < 9:
                clock.now += timeout
                raise TimeoutError("PRIVATE_TIMEOUT_SECRET")
            clock.now += 9
            return io.BytesIO(json.dumps({"total_count": 0, "jobs": []}).encode())
        api = watchdog.GitHubRun("owner/repo", "123", "test-token", opener=slow,
                                 sleep=clock.sleep, monotonic=lambda: clock.now)
        self.assertEqual(api.jobs(), [])
        self.assertEqual(clock.now, 9)

    def test_own_api_failure_fails_open(self):
        def read_outage(request, _timeout):
            return URLError("PRIVATE_SECRET") if request.get_method() == "GET" else {}
        result, transport, stderr = self.run_main([read_outage] * 100)
        self.assertEqual(result, 0)
        self.assertIn("::warning::CI watchdog stopped watching", stderr)
        self.assertIn("recovery budget", stderr)
        self.assertNotIn("PRIVATE_SECRET", stderr)
        self.assertNotIn("test-token", stderr)
        self.assertTrue(all(r.get_method() == "GET" for r, _ in transport.requests))
        self.assertLess(len(transport.requests), 100)

    def test_a_job_that_never_got_a_runner_is_not_a_failure(self):
        # Seen on macOS jobs that queued ~15 min without a runner: completed, cancelled, no runner, no steps.
        never = job("native / mac", conclusion="cancelled", runner_name="")
        running = job("go", "in_progress", None)
        api, transport, clock = self.client([
            {"total_count": 2, "jobs": [never, running]},
            {"total_count": 2, "jobs": [dict(never, runner_name=None), job("go")]},
            {"total_count": 5, "jobs": [job("changes"), job("docs"), job("ci-work-complete"),
                                        dict(never, runner_name=None), job("go")]},
        ])
        result = watchdog.watch(api, sleep=clock.sleep, monotonic=lambda: clock.now, max_seconds=100)
        self.assertEqual(result, "complete")
        self.assertTrue(all(r.get_method() == "GET" for r, _ in transport.requests))

    def test_an_e2e_shard_failure_does_not_cancel_the_run(self):
        shard = job("desktop / desktop-client-linux-x64-3", conclusion="failure")
        voice = job("desktop / desktop-voice-mac-arm64", "in_progress", None,
                    [{"name": "voice", "status": "completed", "conclusion": "failure"}])
        roots = [job("changes"), job("docs"), job("ci-work-complete")]
        api, transport, clock = self.client([
            {"total_count": 5, "jobs": roots + [shard, voice]},
            {"total_count": 5, "jobs": roots + [shard, dict(voice, status="completed", conclusion="failure")]},
        ])
        result = watchdog.watch(api, sleep=clock.sleep, monotonic=lambda: clock.now, max_seconds=100)
        self.assertEqual(result, "complete")
        self.assertEqual(clock.waits, [5])
        self.assertTrue(all(r.get_method() == "GET" for r, _ in transport.requests))

    def test_a_build_failure_still_cancels(self):
        result, transport, _ = self.watch([
            {"total_count": 2, "jobs": [job("go-api-1", conclusion="failure"), job("go", "in_progress", None)]},
            {},
        ])
        self.assertEqual(result, "cancelled")
        self.assertEqual(transport.requests[-1][0].get_method(), "POST")

    def test_retry_after_is_respected_without_exceeding_read_budget(self):
        for seconds in [12, 3600]:
            error = HTTPError("https://api.github.com", 429, "PRIVATE_SECRET", {"Retry-After": str(seconds)}, io.BytesIO())
            api, transport, clock = self.client([error, {"total_count": 0, "jobs": []}])
            if seconds < watchdog.JOBS_READ_BUDGET:
                self.assertEqual(api.jobs(), [])
                self.assertEqual(clock.waits, [12])
                self.assertEqual(len(transport.requests), 2)
            else:
                with self.assertRaisesRegex(RuntimeError, "recovery budget.*HTTP 429"):
                    api.jobs()
                self.assertEqual(clock.now, watchdog.JOBS_READ_BUDGET)
                self.assertEqual(len(transport.requests), 1)

    def test_definitive_auth_and_malformed_responses_do_not_enter_read_recovery(self):
        for code in [401, 403, 404]:
            error = HTTPError("https://api.github.com", code, "PRIVATE_SECRET", {}, io.BytesIO(b"PRIVATE_SECRET"))
            api, transport, clock = self.client([error])
            with self.assertRaisesRegex(RuntimeError, f"HTTP {code}"):
                api.jobs()
            self.assertEqual(clock.waits, [])
            self.assertEqual(len(transport.requests), 1)
        clock = Clock()
        calls = []
        def malformed(request, timeout):
            calls.append(request)
            return io.BytesIO(b'PRIVATE_SECRET not JSON')
        api = watchdog.GitHubRun("owner/repo", "123", "test-token", opener=malformed,
                                 sleep=clock.sleep, monotonic=lambda: clock.now)
        with self.assertRaisesRegex(RuntimeError, "malformed JSON") as caught:
            api.jobs()
        self.assertEqual(clock.waits, [])
        self.assertEqual(len(calls), 1)
        self.assertNotIn("PRIVATE_SECRET", str(caught.exception))
        api, transport, clock = self.client([{"total_count": 1, "jobs": "PRIVATE_SECRET"}])
        with self.assertRaisesRegex(RuntimeError, "malformed jobs response"):
            api.jobs()
        self.assertEqual(clock.waits, [])
        self.assertEqual(len(transport.requests), 1)

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
