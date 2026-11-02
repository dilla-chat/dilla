import io
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from contextlib import redirect_stdout
from pathlib import Path

import provider_harness as harness


class ProcessTests(unittest.TestCase):
    @staticmethod
    def _active_process(pid):
        status = Path(f"/proc/{pid}/stat")
        if status.exists():
            return status.read_text().split()[2] != "Z"
        try:
            os.kill(pid, 0)
            return True
        except ProcessLookupError:
            return False

    @classmethod
    def _spawn_parent_and_descendant(cls, temporary, *, parent_waits, child_ignores_term):
        pid_file = Path(temporary) / "descendant.pid"
        ignore = "signal.signal(signal.SIGTERM, signal.SIG_IGN); " if child_ignores_term else ""
        child = (
            "import signal,sys,time; from pathlib import Path; "
            f"{ignore}Path(sys.argv[1]).write_text(str(__import__('os').getpid())); time.sleep(1.5)"
        )
        parent_wait = "time.sleep(30)" if parent_waits else "pass"
        parent = (
            "import subprocess,sys,time; from pathlib import Path; "
            f"subprocess.Popen([sys.executable, '-c', {child!r}, sys.argv[1]]); "
            "p=Path(sys.argv[1]); "
            "deadline=time.monotonic()+1; "
            "exec('while not p.exists() and time.monotonic()<deadline: time.sleep(0.005)'); "
            f"{parent_wait}"
        )
        return pid_file, [sys.executable, "-c", parent, str(pid_file)]

    @classmethod
    def _read_pid(cls, pid_file):
        deadline = time.monotonic() + 1
        while not pid_file.exists() and time.monotonic() < deadline:
            time.sleep(0.005)
        return int(pid_file.read_text())

    @classmethod
    def _stop_test_descendant(cls, pid):
        if cls._active_process(pid):
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass

    def test_missing_executable_is_reported_without_raw_error(self):
        with self.assertRaises(harness.HarnessError) as caught:
            harness.run_bounded(["/definitely/missing/provider-tool"], timeout=1)
        self.assertEqual(caught.exception.kind, "unavailable")
        self.assertNotIn("/definitely/missing", str(caught.exception))

    def test_nonzero_version_command_is_not_available(self):
        result = harness.probe_tool("yt-dlp", "/bin/false")
        self.assertFalse(result.available)
        self.assertEqual(result.reason, "version command failed")

    def test_timeout_terminates_and_reaps_child(self):
        with self.assertRaises(harness.HarnessError) as caught:
            harness.run_bounded(["python3", "-c", "import time; time.sleep(5)"], timeout=0.1)
        self.assertEqual(caught.exception.kind, "timeout")

    def test_timeout_stays_bounded_when_exited_parent_child_holds_stdout(self):
        with tempfile.TemporaryDirectory() as temporary:
            pid_file, argv = self._spawn_parent_and_descendant(
                temporary, parent_waits=False, child_ignores_term=False
            )
            started = time.monotonic()
            try:
                with self.assertRaises(harness.HarnessError) as caught:
                    harness.run_bounded(argv, timeout=0.15)
                elapsed = time.monotonic() - started
                descendant_pid = self._read_pid(pid_file)
                self.assertEqual(caught.exception.kind, "timeout")
                self.assertLess(elapsed, 0.7)
                self.assertFalse(self._active_process(descendant_pid))
            finally:
                if pid_file.exists():
                    self._stop_test_descendant(int(pid_file.read_text()))

    def test_timeout_kills_sigterm_ignoring_descendant_after_parent_exits(self):
        with tempfile.TemporaryDirectory() as temporary:
            pid_file, argv = self._spawn_parent_and_descendant(
                temporary, parent_waits=True, child_ignores_term=True
            )
            try:
                with self.assertRaises(harness.HarnessError) as caught:
                    harness.run_bounded(argv, timeout=0.15)
                descendant_pid = self._read_pid(pid_file)
                deadline = time.monotonic() + 0.5
                while self._active_process(descendant_pid) and time.monotonic() < deadline:
                    time.sleep(0.01)
                self.assertEqual(caught.exception.kind, "timeout")
                self.assertFalse(self._active_process(descendant_pid), "timed-out descendant is still running")
            finally:
                if pid_file.exists():
                    self._stop_test_descendant(int(pid_file.read_text()))

    def test_output_limit_terminates_and_reaps_child(self):
        with self.assertRaises(harness.HarnessError) as caught:
            harness.run_bounded(
                ["python3", "-c", "import sys; sys.stdout.write('x' * 1000000)"],
                timeout=2,
                output_limit=64,
            )
        self.assertEqual(caught.exception.kind, "output limit")
        self.assertLessEqual(len(caught.exception.output), 64)


class DoctorTests(unittest.TestCase):
    def test_cli_checks_youtube_prerequisites_without_audio_server_dependency(self):
        output = io.StringIO()
        with redirect_stdout(output):
            code = harness.main([
                "doctor",
                "--yt-dlp-bin", "/bin/true", "--ffmpeg-bin", "/bin/true",
                "--ffprobe-bin", "/bin/true", "--js-runtime-bin", "/bin/true",
            ])
        self.assertEqual(code, 0)
        self.assertIn("yt-dlp: available", output.getvalue())
        self.assertNotIn("soloist", output.getvalue())
        self.assertNotIn("audio-tool", output.getvalue())
        self.assertIn("provider qualification: unverified", output.getvalue())

    def test_doctor_returns_nonzero_when_prerequisite_missing_and_states_unverified(self):
        output = io.StringIO()
        with redirect_stdout(output):
            code = harness.doctor({name: "/definitely/missing" for name in harness.DOCTOR_TOOLS})
        self.assertNotEqual(code, 0)
        self.assertIn("provider qualification: unverified", output.getvalue())
        self.assertNotIn("/definitely/missing", output.getvalue())

    def test_version_text_is_reduced_to_version_token(self):
        with tempfile.TemporaryDirectory() as temporary:
            fake = Path(temporary) / "tool"
            fake.write_text("#!/bin/sh\necho 'tool version 4.5.6 /home/private/token=secret'\n")
            fake.chmod(0o700)
            result = harness.probe_tool("yt-dlp", str(fake))
        self.assertTrue(result.available)
        self.assertEqual(result.version, "4.5.6")


class OpusTests(unittest.TestCase):
    def test_encode_failure_is_reported(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / "input.ogg"
            with self.assertRaises(harness.HarnessError) as caught:
                harness.encode_opus("/bin/false", source, duration=0.2)
        self.assertEqual(caught.exception.kind, "command failed")

    def test_malformed_probe_json_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            audio = Path(temporary) / "audio.ogg"
            audio.write_bytes(b"not opus")
            fake = Path(temporary) / "ffprobe"
            fake.write_text("#!/bin/sh\necho 'not json'\n")
            fake.chmod(0o700)
            with self.assertRaises(harness.HarnessError) as caught:
                harness.validate_opus(str(fake), audio)
        self.assertEqual(caught.exception.kind, "invalid stream")

    def test_empty_packet_list_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            audio = Path(temporary) / "audio.ogg"
            audio.write_bytes(b"nonempty")
            fake = Path(temporary) / "ffprobe"
            payload = {"streams": [{"codec_name": "opus", "sample_rate": "48000", "channels": 2}], "packets": []}
            fake.write_text("#!/bin/sh\ncat <<'EOF'\n" + json.dumps(payload) + "\nEOF\n")
            fake.chmod(0o700)
            with self.assertRaises(harness.HarnessError) as caught:
                harness.validate_opus(str(fake), audio)
        self.assertEqual(caught.exception.kind, "invalid stream")

    def test_packet_evidence_must_be_20ms_stereo_96kbps_opus(self):
        with tempfile.TemporaryDirectory() as temporary:
            audio = Path(temporary) / "audio.ogg"
            audio.write_bytes(b"nonempty")
            fake = Path(temporary) / "ffprobe"
            payload = {
                "streams": [{"codec_name": "opus", "sample_rate": "48000", "channels": 2}],
                "packets": [{"size": "240", "duration_time": "0.020000"} for _ in range(5)],
            }
            fake.write_text("#!/bin/sh\ncat <<'EOF'\n" + json.dumps(payload) + "\nEOF\n")
            fake.chmod(0o700)
            self.assertEqual(harness.validate_opus(str(fake), audio), 5)

    def test_wrong_packet_size_is_rejected_as_wrong_cbr_rate(self):
        with tempfile.TemporaryDirectory() as temporary:
            audio = Path(temporary) / "audio.ogg"
            audio.write_bytes(b"nonempty")
            fake = Path(temporary) / "ffprobe"
            payload = {
                "streams": [{"codec_name": "opus", "sample_rate": "48000", "channels": 2}],
                "packets": [{"size": "241", "duration_time": "0.020000"} for _ in range(5)],
            }
            fake.write_text("#!/bin/sh\ncat <<'EOF'\n" + json.dumps(payload) + "\nEOF\n")
            fake.chmod(0o700)
            with self.assertRaises(harness.HarnessError) as caught:
                harness.validate_opus(str(fake), audio)
        self.assertEqual(caught.exception.kind, "invalid stream")


if __name__ == "__main__":
    unittest.main()
