package youtube

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProcessError retains bounded subprocess diagnostics without putting command
// arguments (which may contain message content) in Error().
type ProcessError struct {
	Cause    error
	Stderr   string
	ExitCode int
	class    string
}

func (e *ProcessError) Error() string { return e.Cause.Error() }
func (e *ProcessError) Unwrap() error { return e.Cause }
func (e *ProcessError) PermanentPlaybackFailure() bool {
	if errors.Is(e.Cause, context.Canceled) || errors.Is(e.Cause, context.DeadlineExceeded) {
		return false
	}
	switch e.class {
	case "video_unavailable", "video_private", "video_removed", "video_region_restricted", "video_age_restricted", "video_too_long":
		return true
	}
	return false
}

// PermanentPlaybackFailure is deliberately limited to validated track input,
// metadata, and bounded yt-dlp video refusal classes. Host/provider faults pause.
func PermanentPlaybackFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var process *ProcessError
	if errors.As(err, &process) && process.class != "" {
		return process.PermanentPlaybackFailure()
	}
	return errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrMetadataInvalid)
}

// processFailureClass inspects only bounded subprocess diagnostics and returns
// fixed labels. Provider-wide refusals take precedence over generic video text.
func processFailureClass(raw string) string {
	text := strings.ToLower(raw[:min(len(raw), 4096)])
	contains := func(markers ...string) bool {
		for _, marker := range markers {
			if strings.Contains(text, marker) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("http error 429", "http 429", "too many requests"):
		return "http_429"
	case contains("not a bot", "bot check", "bot-check", "automated requests"):
		return "bot_check"
	case contains("http error 403", "http 403"):
		return "http_403"
	case contains("name resolution", "name or service not known", "network is unreachable", "connection refused", "connection reset", "timed out", "temporary failure", "unable to download webpage", "unable to download api page"):
		return "network"
	case contains("try again later", "content isn't available", "content isn’t available"):
		return "rate_limited"
	case contains("video is private", "private video"):
		return "video_private"
	case contains("video has been removed", "video was removed", "video has been deleted", "video is no longer available"):
		return "video_removed"
	case contains("not available in your country", "not made this video available in your country", "not available in your region", "geo restricted", "geo-restricted"):
		return "video_region_restricted"
	case contains("confirm your age", "age-restricted", "age restricted", "inappropriate for some users"):
		return "video_age_restricted"
	case contains("video is too long", "video duration exceeds", "longer than maximum duration"):
		return "video_too_long"
	case contains("video unavailable", "this video is unavailable"):
		return "video_unavailable"
	}
	return ""
}

// DiagnosticText produces one printable line, at most 512 UTF-8 bytes including
// the truncation marker. Whitespace separates words; other controls are removed.
func DiagnosticText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(value, ""))
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= 512 {
		return value
	}
	end := 512 - len("…")
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "…"
}

// metadataError exposes only a fixed validation reason, never extractor output.
// It preserves the public sentinel for callers using errors.Is.
type metadataError struct{ reason string }

func (e *metadataError) Error() string    { return "youtube: invalid_metadata:" + e.reason }
func (e *metadataError) Unwrap() error    { return ErrMetadataInvalid }
func invalidMetadata(reason string) error { return &metadataError{reason: reason} }

func FailureClass(err error) string {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		var process *ProcessError
		if errors.As(err, &process) && process.class != "" {
			return process.class
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrSandboxUnavailable):
		return "sandbox_unavailable"
	case errors.Is(err, ErrExtractFailed):
		return "extract_failed"
	case errors.Is(err, ErrMetadataInvalid):
		var metadata *metadataError
		if errors.As(err, &metadata) {
			return "invalid_metadata:" + metadata.reason
		}
		return "metadata_invalid"
	case errors.Is(err, ErrInvalidInput):
		return "invalid_input"
	case errors.Is(err, ErrMetadataTooLarge):
		return "metadata_too_large"
	case errors.Is(err, ErrRunnerRequired):
		return "runner_required"
	}
	var process *ProcessError
	if errors.As(err, &process) && process.ExitCode != 0 {
		return "exit_" + strconv.Itoa(process.ExitCode)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "exit_" + strconv.Itoa(exit.ExitCode())
	}
	return "unknown"
}

func FailureStderr(err error) string {
	var process *ProcessError
	if errors.As(err, &process) {
		return DiagnosticText(process.Stderr)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return DiagnosticText(string(exit.Stderr))
	}
	return ""
}

type diagnosticLoggerKey struct{}

// WithDiagnosticLogger carries installation-scoped logging into the sandbox.
func WithDiagnosticLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, diagnosticLoggerKey{}, logger)
}

type diagnosticInputKey struct{}

var diagnosticURL = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s]+`)
var diagnosticYouTubeToken = regexp.MustCompile(`(\[youtube[^\]]*\]\s+)[^\r\n]*?:`)

// Redact raw text before normalization or truncation can split a private value.
func newProcessError(cause error, raw string, exitCode int, inputs ...string) *ProcessError {
	class := ""
	if exitCode != 0 {
		class = processFailureClass(raw)
	}
	var secrets []string
	for _, input := range inputs {
		if input == "" {
			continue
		}
		secrets = append(secrets, input)
		if kind, canonical, err := NormalizeInput(input); err == nil {
			secrets = append(secrets, canonical)
			if kind == "search" {
				secrets = append(secrets, strings.TrimPrefix(canonical, "ytsearch5:"))
			} else {
				u, _ := url.Parse(canonical)
				secrets = append(secrets, u.Query().Get("v"), u.Query().Get("list"))
			}
		}
	}
	var encoded []string
	for _, secret := range secrets {
		if secret != "" {
			encoded = append(encoded, secret, url.QueryEscape(secret), url.PathEscape(secret))
		}
	}
	sort.Slice(encoded, func(i, j int) bool { return len(encoded[i]) > len(encoded[j]) })
	patterns := []string{diagnosticURL.String()}
	for _, secret := range encoded {
		patterns = append(patterns, regexp.QuoteMeta(secret))
	}
	pattern := regexp.MustCompile(strings.Join(patterns, "|"))
	// Replace URLs and known secrets in one pass so short queries cannot rewrite
	// markers or strip a URL scheme before the URL matcher sees it.
	raw = pattern.ReplaceAllString(raw, "[redacted]")
	// Known search text (which can contain colons) is gone before the generic
	// extractor token fallback handles IDs belonging to other playlist videos.
	raw = diagnosticYouTubeToken.ReplaceAllString(raw, "${1}[redacted]:")
	return &ProcessError{Cause: cause, Stderr: DiagnosticText(raw), ExitCode: exitCode, class: class}
}
