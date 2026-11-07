package client

import (
	"context"
	"net/http"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
)

// HealthReport is what the Bot tells dillad about itself (PUT /v1/bot-runtime/health): whether its own command path
// answered the last probe, when it probed, its backlog, and since when commands fail. See internal/bot/health.
type HealthReport struct {
	ProbeOK       bool
	ProbeAt       time.Time
	QueueDepth    uint64
	OldestWaitMS  uint64
	MusicStartP95 uint64
	FailingSince  *time.Time
}

// ReportHealth sends one health report. dillad judges it (degraded after two failed probes, a command waiting over
// 10 s or a queue growing for 60 s; offline when the reports stop for 150 s).
func (s *Session) ReportHealth(ctx context.Context, r HealthReport) error {
	if s == nil || s.c == nil || s.token == "" {
		return ErrInvalidConfig
	}
	var ok, probeAt uint64
	if r.ProbeOK {
		ok = 1
	}
	if r.ProbeAt.Unix() > 0 {
		probeAt = uint64(r.ProbeAt.Unix()) //nolint:gosec // checked positive
	}
	var failing any
	if r.FailingSince != nil && r.FailingSince.Unix() > 0 {
		failing = uint64(r.FailingSince.Unix()) //nolint:gosec // checked positive
	}
	body, err := cborx.Marshal([]any{ok, probeAt, r.QueueDepth, r.OldestWaitMS, r.MusicStartP95, failing})
	if err != nil {
		return err
	}
	_, _, err = s.do(ctx, http.MethodPut, "bot-runtime/health", nil, body, MaxSmallBody, true)
	return err
}
