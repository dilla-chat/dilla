package youtube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestYouTubeDiagnosticTextIsBoundedAndPrintable(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"namespace\n\t failed\x00\x1b\u202e", "namespace failed"},
		{strings.Repeat("x", 600), strings.Repeat("x", 509) + "…"},
		{strings.Repeat("é", 600), strings.Repeat("é", 254) + "…"},
	} {
		got := DiagnosticText(tc.input)
		if got != tc.want || len(got) > 512 || !utf8.ValidString(got) {
			t.Fatalf("diagnostic=%q (%d bytes)", got, len(got))
		}
	}
}
func TestYouTubeProcessFailureClasses(t *testing.T) {
	for _, tc := range []struct {
		err   error
		class string
	}{
		{ErrSandboxUnavailable, "sandbox_unavailable"}, {ErrExtractFailed, "extract_failed"}, {ErrMetadataInvalid, "metadata_invalid"},
		{ErrInvalidInput, "invalid_input"}, {context.DeadlineExceeded, "deadline_exceeded"}, {context.Canceled, "canceled"},
		{ErrMetadataTooLarge, "metadata_too_large"}, {ErrRunnerRequired, "runner_required"},
	} {
		err := &ProcessError{Cause: tc.err, Stderr: "bwrap\nfailed", ExitCode: 1}
		if !errors.Is(err, tc.err) || FailureClass(err) != tc.class || FailureStderr(err) != "bwrap failed" || strings.Contains(err.Error(), "bwrap") {
			t.Fatalf("error=%v class=%s stderr=%s", err, FailureClass(err), FailureStderr(err))
		}
	}
}

func TestYouTubeProcessExitCodeClass(t *testing.T) {
	err := &ProcessError{Cause: errors.New("process exited"), ExitCode: 42}
	if class := FailureClass(err); class != "exit_42" {
		t.Fatalf("class=%s", class)
	}
}

func TestIsolatedMetadataRunnerRetainsSandboxStderr(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf 'namespace\\nblocked\\000' >&2\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("installation_id", "test-installation")
	ctx := WithDiagnosticLogger(t.Context(), logger)
	_, err = (IsolatedMetadataRunner{Bubblewrap: shim, Python: python}).Run(ctx, executable, nil, 2048)
	var process *ProcessError
	if !errors.Is(err, ErrSandboxUnavailable) || !errors.As(err, &process) || process.ExitCode != 42 || FailureStderr(err) != "namespace blocked" {
		t.Fatalf("error=%v diagnostics=%q", err, FailureStderr(err))
	}
	if logs.Len() != 0 {
		t.Fatalf("startup log=%s", logs.String())
	}
}

func TestSandboxStderrCaptureIsBounded(t *testing.T) {
	capture := &sandboxStderr{}
	input := strings.Repeat("x", 100000)
	n, err := capture.Write([]byte(input))
	if err != nil || n != len(input) || len(capture.String()) > 4096 || len(DiagnosticText(capture.String())) != 512 {
		t.Fatalf("write=%d err=%v captured=%d", n, err, len(capture.String()))
	}
}

func TestProcessErrorRedactsRawUserContent(t *testing.T) {
	query := "private  query & song"
	raw := query + " " + url.QueryEscape(query) + " " + url.PathEscape(query) + " https://elsewhere.test/private [youtube] ZYXwvutsrqp: failed [youtube:search] " + query + ": failed"
	err := newProcessError(ErrExtractFailed, raw, 1, query)
	want := "[redacted] [redacted] [redacted] [redacted] [youtube] [redacted]: failed [youtube:search] [redacted]: failed"
	if got := FailureStderr(err); got != want {
		t.Fatalf("stderr=%q", got)
	}
	for _, input := range []string{"https://youtu.be/aqz-KE-bpKQ", "https://youtube.com/playlist?list=PLsecret123"} {
		_, canonical, _ := NormalizeInput(input)
		id := "aqz-KE-bpKQ"
		if strings.Contains(input, "playlist") {
			id = "PLsecret123"
		}
		err := newProcessError(ErrExtractFailed, input+" "+canonical+" "+id, 1, input)
		if got := FailureStderr(err); got != "[redacted] [redacted] [redacted]" {
			t.Fatalf("stderr=%q", got)
		}
	}
	// Redaction must precede whitespace normalization and truncation.
	err = newProcessError(ErrExtractFailed, strings.Repeat("x", 500)+" "+query, 1, query)
	if strings.Contains(FailureStderr(err), "private") {
		t.Fatal("truncated query leaked")
	}
}

func TestRedactionDoesNotRewriteMarkersOrURLSchemes(t *testing.T) {
	for _, query := range []string{"redacted", "https", "a"} {
		err := newProcessError(ErrExtractFailed, query+" https://example.test/private [youtube] ZYXwvutsrqp: failed", 1, query)
		want := "[redacted] [redacted] [youtube] [redacted]: f"
		// Compare the content-bearing prefix; a one-letter query also redacts that
		// letter in ordinary diagnostic words.
		if got := FailureStderr(err); !strings.HasPrefix(got, want) {
			t.Fatalf("query=%q stderr=%q", query, got)
		}
	}
}

func TestRedactionCoversSearchContainingColon(t *testing.T) {
	query := "find this: private song"
	err := newProcessError(ErrExtractFailed, "[youtube:search] "+query+": failed", 1, query)
	if got := FailureStderr(err); got != "[youtube:search] [redacted]: failed" {
		t.Fatalf("stderr=%q", got)
	}
}

func TestYouTubeProcessClassifiesPermanentVideoFailures(t *testing.T) {
	for _, tc := range []struct {
		raw, class string
		permanent  bool
	}{
		{"ERROR: Video unavailable", "video_unavailable", true},
		{"ERROR: [youtube] id: Video unavailable. This content isn't available, try again later.", "rate_limited", false},
		{"ERROR: Video unavailable. This content isn’t available, try again later.", "rate_limited", false},
		{"ERROR: Video unavailable. This content isn't available", "rate_limited", false},
		{"ERROR: Video unavailable. This content isn’t available", "rate_limited", false},
		{"ERROR: This video is private", "video_private", true},
		{"ERROR: This video has been removed by the uploader", "video_removed", true},
		{"ERROR: The uploader has not made this video available in your country", "video_region_restricted", true},
		{"ERROR: Sign in to confirm your age", "video_age_restricted", true},
		{"ERROR: Video is too long", "video_too_long", true},
		{"ERROR: HTTP Error 429: Too Many Requests; Video unavailable", "http_429", false},
		{"ERROR: Sign in to confirm you're not a bot; Video unavailable", "bot_check", false},
		{"ERROR: HTTP Error 403: Forbidden", "http_403", false},
		{"ERROR: Unable to download webpage: Temporary failure in name resolution", "network", false},
		{"bwrap: namespace permission denied", "sandbox_unavailable", false},
	} {
		t.Run(tc.class, func(t *testing.T) {
			failure := newProcessError(ErrSandboxUnavailable, tc.raw, 1)
			if FailureClass(failure) != tc.class {
				t.Fatalf("class=%s want %s", FailureClass(failure), tc.class)
			}
			classified, ok := any(failure).(interface{ PermanentPlaybackFailure() bool })
			if !ok || classified.PermanentPlaybackFailure() != tc.permanent {
				t.Fatalf("permanent=%v want %v", ok && classified.PermanentPlaybackFailure(), tc.permanent)
			}
		})
	}
	canceled := newProcessError(context.Canceled, "Video unavailable", 1)
	if classified, ok := any(canceled).(interface{ PermanentPlaybackFailure() bool }); ok && classified.PermanentPlaybackFailure() {
		t.Fatal("canceled yt-dlp must pause")
	}
}

func TestSandboxEarlyExitRetainsVideoOrProviderFailure(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		raw, class string
		permanent  bool
	}{
		{"ERROR: This video is private", "video_private", true},
		{"ERROR: HTTP Error 429: Too Many Requests", "http_429", false},
		{"bwrap: namespace permission denied", "sandbox_unavailable", false},
	} {
		t.Run(tc.class, func(t *testing.T) {
			shim := filepath.Join(t.TempDir(), "bwrap")
			script := "#!/bin/sh\ncat >&2 <<'DIAGNOSTIC'\n" + tc.raw + "\nDIAGNOSTIC\nexit 1\n"
			if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			_, failure := launchNetworkWorker(t.Context(), shim, python, nil, []string{"pcm", "", "", "", "https://youtu.be/aqz-KE-bpKQ"}, time.Second, time.Minute)
			if FailureClass(failure) != tc.class {
				t.Fatalf("class=%s want %s: %v", FailureClass(failure), tc.class, failure)
			}
			if tc.permanent && errors.Is(failure, ErrSandboxUnavailable) {
				t.Fatal("video refusal mislabeled as sandbox setup failure")
			}
			capability, ok := failure.(interface{ PermanentPlaybackFailure() bool })
			if (ok && capability.PermanentPlaybackFailure()) != tc.permanent {
				t.Fatalf("permanent=%v want %v", ok && capability.PermanentPlaybackFailure(), tc.permanent)
			}
		})
	}
}

type closedStdoutReader struct{}

func (closedStdoutReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

func TestPCMReadClosedPipePreservesCompletedVideoFailure(t *testing.T) {
	capture := &sandboxStderr{}
	_, _ = capture.Write([]byte("ERROR: This video is private"))
	done := make(chan struct{})
	close(done)
	process := &isolatedPCM{stdout: io.NopCloser(closedStdoutReader{}), cmd: &exec.Cmd{Stderr: capture}, done: done, err: errors.New("child exited"), exitCode: 1}
	reader := &boundedProcessPCM{process: process, limit: 100}
	var sample [4]byte
	_, err := reader.Read(sample[:])
	var failure *ProcessError
	if !errors.As(err, &failure) || !errors.Is(err, ErrPCMStart) || !failure.PermanentPlaybackFailure() || FailureClass(err) != "video_private" {
		t.Fatalf("closed pipe lost process failure: %v class=%s", err, FailureClass(err))
	}
}
