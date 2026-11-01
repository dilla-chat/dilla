package livekit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/jonasthim/dilla/internal/bot/music"
)

// classifiedFailure adds only a bounded diagnostic class; the original typed
// cause remains available to errors.Is/As and is never written to the logger.
type classifiedFailure struct {
	error
	class string
}

func (e *classifiedFailure) Error() string        { return "music livekit: " + e.class }
func (e *classifiedFailure) Unwrap() error        { return e.error }
func (e *classifiedFailure) FailureClass() string { return e.class }

func failureClass(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) {
		return "ctx_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	var classified interface{ FailureClass() string }
	if errors.As(err, &classified) {
		return classified.FailureClass()
	}
	switch {
	case errors.Is(err, ErrBinding):
		return "binding"
	case errors.Is(err, ErrEpochChanged):
		return "epoch_changed"
	case errors.Is(err, ErrPoolDead):
		return "pool_dead"
	case errors.Is(err, ErrPoolBusy):
		return "pool_busy"
	case errors.Is(err, ErrPoolClosed):
		return "pool_closed"
	case errors.Is(err, ErrPoolIdle):
		return "idle"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var tlsHeader tls.RecordHeaderError
	var cert *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &tlsHeader) || errors.As(err, &cert) || errors.As(err, &unknownCA) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return "tls"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	if errors.Is(err, lksdk.ErrConnectionTimeout) || errors.Is(err, lksdk.ErrTrackPublishTimeout) {
		return "timeout"
	}
	var op *net.OpError
	if errors.As(err, &op) && strings.HasPrefix(op.Net, "tcp") {
		return "tcp"
	}
	// The SDK and ICE stack also use untyped errors. Match only fixed transport
	// markers, and return a fixed enum rather than any part of the message.
	text := strings.ToLower(err.Error())
	if strings.HasPrefix(text, "ice ") || strings.Contains(text, ": ice ") || strings.Contains(text, "ice:") {
		return "ice"
	}
	if strings.Contains(text, "tls:") {
		return "tls"
	}
	if errors.Is(err, ErrRoomConnection) {
		return "room_connection"
	}
	return "invalid"
}

func classified(err error) error {
	if err == nil {
		return nil
	}
	if class := failureClass(err); class != "invalid" {
		return &classifiedFailure{error: err, class: class}
	}
	return err
}

func startStage(ctx context.Context, stage string, err error) error {
	return music.StartStage(ctx, stage, classified(err))
}
func connectionFailure(err error) error {
	return &classifiedFailure{error: fmt.Errorf("%w: %w", ErrRoomConnection, err), class: roomFailureClass(err)}
}
func roomFailureClass(err error) string {
	if class := failureClass(err); class != "invalid" {
		return class
	}
	return "room_connection"
}

// sdkLogger deliberately drops SDK text, values, names and deferred fields:
// they may contain tokens, SDP, URLs, addresses or response bodies. Each room
// retains the playback logger's IDs, and only SDK Warn/Error reach that logger.
type sdkLogger struct {
	protoLogger.Logger
	ctx   context.Context
	mu    sync.Mutex
	class string
	cause error
}

// detachedLoggerContext carries room logging identity without retaining attempt values or cancellation.
func detachedLoggerContext(ctx context.Context) context.Context {
	return music.WithPlaybackGroup(music.WithPlaybackLogger(context.Background(), music.PlaybackLogger(ctx)), music.PlaybackGroup(ctx))
}

func newSDKLogger(ctx context.Context) *sdkLogger {
	return &sdkLogger{Logger: protoLogger.GetDiscardLogger(), ctx: detachedLoggerContext(ctx)}
}
func (*sdkLogger) Debugw(string, ...any)                       {}
func (*sdkLogger) Infow(string, ...any)                        {}
func (l *sdkLogger) Warnw(_ string, err error, fields ...any)  { l.write(false, err, fields) }
func (l *sdkLogger) Errorw(_ string, err error, fields ...any) { l.write(true, err, fields) }
func (l *sdkLogger) write(isError bool, err error, fields []any) {
	class := failureClass(err)
	for i := 0; i+1 < len(fields); i += 2 {
		if key, ok := fields[i].(string); ok && key == "status" {
			if status, ok := fields[i+1].(int); ok && status >= 100 && status <= 599 {
				class = fmt.Sprintf("http_%d", status)
			}
		}
	}
	if err != nil || class != "ok" {
		l.mu.Lock()
		if l.cause == nil || (class != "invalid" && class != "ok") {
			l.class, l.cause = class, err
		}
		l.mu.Unlock()
	}
	logger := music.PlaybackLogger(l.ctx)
	if isError {
		logger.ErrorContext(l.ctx, "bot music LiveKit SDK", "class", class)
	} else {
		logger.WarnContext(l.ctx, "bot music LiveKit SDK", "class", class)
	}
}
func (l *sdkLogger) wrap(err error) error {
	if err == nil {
		return nil
	}
	l.mu.Lock()
	class, cause := l.class, l.cause
	l.mu.Unlock()
	// The SDK replaces dial errors with /validate failures. Retain both
	// so callers can still inspect the original typed transport cause.
	if cause != nil {
		err = errors.Join(err, cause)
	}
	if class == "" || class == "invalid" || class == "ok" {
		return err
	}
	return &classifiedFailure{error: err, class: class}
}
func (l *sdkLogger) WithValues(...any) protoLogger.Logger { return l }
func (l *sdkLogger) WithUnlikelyValues(...any) protoLogger.UnlikelyLogger {
	return protoLogger.NewUnlikelyLogger(l)
}
func (l *sdkLogger) WithName(string) protoLogger.Logger      { return l }
func (l *sdkLogger) WithComponent(string) protoLogger.Logger { return l }
func (l *sdkLogger) WithCallDepth(int) protoLogger.Logger    { return l }
func (l *sdkLogger) WithItemSampler() protoLogger.Logger     { return l }
func (l *sdkLogger) WithoutSampler() protoLogger.Logger      { return l }
func (l *sdkLogger) WithDeferredValues() (protoLogger.Logger, protoLogger.DeferredFieldResolver) {
	_, resolver := l.Logger.WithDeferredValues()
	return l, resolver
}

func logRoomEnded(ctx context.Context, cause error) {
	if ctx == nil {
		return
	}
	reason := failureClass(cause)
	switch {
	case errors.Is(cause, ErrPoolDead):
		reason = "disconnect"
	case errors.Is(cause, ErrPoolIdle):
		reason = "idle"
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "lifetime"
	case errors.Is(cause, ErrEpochChanged):
		reason = "epoch"
	}
	logger := music.PlaybackLogger(ctx)
	if errors.Is(cause, ErrPoolIdle) {
		logger.InfoContext(ctx, "bot music room ended", "reason", reason, "class", failureClass(cause))
	} else {
		logger.WarnContext(ctx, "bot music room ended", "reason", reason, "class", failureClass(cause))
	}
}
