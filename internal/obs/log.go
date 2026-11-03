// Package obs is dillad's observability: a redacting JSON logger, the
// Prometheus registry and the two health endpoints.
//
// The redactor exists because dillad handles bearer tokens, password hashes,
// TOTP secrets and DSNs, and a log line is the easiest place in the system to
// leak one. It replaces by KEY rather than by value, at any depth, because the
// value of a secret is exactly what we cannot pattern-match on.
package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/dilla-chat/dilla/internal/config"
)

// secretKeys is matched case-insensitively against the whole attribute key and
// against its last underscore-separated word, so `api_key`, `dsn` and
// `session_token` are all caught.
//
// "code" is deliberately NOT in this list: slog.String("code", "E_NOT_FOUND") is
// the natural way to log a refusal, and redacting it would make every refusal
// log useless. The secret-carrying spellings are named individually instead.
var secretKeys = []string{"token", "password", "secret", "dsn", "phc", "nonce", "sig", "key",
	"invite_code", "recovery_code", "backup_code", "otp_code", "verifier"}

const redacted = "[redacted]"

type redactingHandler struct{ inner slog.Handler }

func (h redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(clean(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cleaned := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		cleaned = append(cleaned, clean(a))
	}
	return redactingHandler{inner: h.inner.WithAttrs(cleaned)}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{inner: h.inner.WithGroup(name)}
}

func clean(a slog.Attr) slog.Attr {
	// Resolve FIRST. A slog.LogValuer is expanded by the handler AFTER the
	// redactor has run, so an unresolved one carries whatever it wants straight
	// into the log — including a group with a token in it. Resolve follows the
	// chain and slog caps the depth.
	if a.Value.Kind() == slog.KindLogValuer {
		a.Value = a.Value.Resolve()
	}
	if a.Value.Kind() == slog.KindGroup {
		inner := a.Value.Group()
		out := make([]slog.Attr, 0, len(inner))
		for _, sub := range inner {
			out = append(out, clean(sub))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	if isSecret(a.Key) {
		return slog.String(a.Key, redacted)
	}
	// A slog.KindAny value marshals through the handler's own encoder, which the
	// redactor cannot see into, so it is redacted by KEY only — which is why
	// config.Redacted() exists and why TestConfigDoesNotImplementLogValuerByAccident
	// (task 4) forbids Config from resolving itself into one.
	if strings.HasSuffix(a.Key, "_id") && a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, prefix8(a.Value.String()))
	}
	// The call routes' own spellings of a device and a call id (SFU-9), as a string or a Stringer
	// such as id.ID, and a LiveKit room "<call id>-<seconds>-<random>".
	if callIDKeys[a.Key] {
		if s, ok := idText(a.Value); ok {
			return slog.String(a.Key, prefix8(s))
		}
	}
	if a.Key == "room" {
		if s, ok := idText(a.Value); ok {
			call, rest, found := strings.Cut(s, "-")
			if found {
				return slog.String(a.Key, prefix8(call)+"-"+rest)
			}
			return slog.String(a.Key, prefix8(call))
		}
	}
	return a
}

// callIDKeys are the keys the call routes log a device or a call id under without the _id suffix.
var callIDKeys = map[string]bool{"identity": true, "device": true, "call": true}

// idText is v's text when it is a string or a fmt.Stringer.
func idText(v slog.Value) (string, bool) {
	if v.Kind() == slog.KindString {
		return v.String(), true
	}
	if v.Kind() == slog.KindAny {
		if s, ok := v.Any().(fmt.Stringer); ok {
			return s.String(), true
		}
	}
	return "", false
}

func isSecret(key string) bool {
	k := strings.ToLower(key)
	for _, s := range secretKeys {
		if k == s || strings.HasSuffix(k, "_"+s) || strings.HasPrefix(k, s+"_") {
			return true
		}
	}
	return false
}

// prefix8 logs an identifier as its first eight hex characters: enough to
// correlate two lines, not enough to enumerate a group's membership out of a
// log shipped to someone else's collector.
func prefix8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// NewLogger returns the configured logger. The redactor wraps the handler, so
// every call site — including third-party code handed this logger — is covered.
func NewLogger(c config.Log, w io.Writer) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.Level)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level, AddSource: c.AddSource}
	var h slog.Handler
	if c.Format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(redactingHandler{inner: h})
}
