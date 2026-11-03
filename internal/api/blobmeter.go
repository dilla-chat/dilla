package api

import (
	"math"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// UploadMeter is the per-user upload meter of [blobs] (fix wave C7): uploads_per_minute is a
// request bucket and upload_bytes_per_day a byte bucket, both token buckets that refill
// continuously and start full. A PUT reserves one request and the bytes it may write before a
// byte of the body is read, and settles the byte reservation to what was actually read once the
// upload is over, so bytes the instance read and then refused (a hash mismatch, a quota refusal
// after the body), orphaned, or later deleted all spend the budget: deleting an attachment frees
// quota, never the day's budget. A zero or negative setting turns its bucket off.
//
// The buckets live in memory: a restart refills them. Their number is bounded by the enrolled
// users who uploaded since the process started.
//
// One meter serves every route that stores uploaded bytes under their hash: the attachment PUT and
// PUT /v1/backups (security review F3), so a user's budget is one budget whichever route spends it.
type UploadMeter struct {
	clk       clock.Clock
	perMinute float64
	perDay    float64

	mu    sync.Mutex
	users map[id.ID]*uploadBuckets
}

type uploadBuckets struct {
	requests, bytes float64
	at              time.Time
}

// NewUploadMeter is the meter blobs.uploads_per_minute and blobs.upload_bytes_per_day configure,
// or nil (which meters nothing) when both are off.
func NewUploadMeter(clk clock.Clock, cfg config.Blobs) *UploadMeter {
	if cfg.UploadsPerMinute <= 0 && cfg.UploadBytesPerDay <= 0 {
		return nil
	}
	return newBlobMeter(clk, cfg.UploadsPerMinute, cfg.UploadBytesPerDay)
}

// StateBytesPerDevicePerDay is the daily byte budget of one device's state-object PUTs: 64 state
// objects at the 1,048,640-byte body cap, far more than honest re-seals need, and a bound on the
// bytes one device can make the instance read and write through the route.
const StateBytesPerDevicePerDay = 64 << 20

// NewStateMeter is the per-device state-object meter: a byte bucket of bytesPerDay that refills
// continuously, keyed by device id, with no request bucket (the route's device write bucket meters
// requests).
func NewStateMeter(clk clock.Clock, bytesPerDay int64) *UploadMeter {
	return newBlobMeter(clk, 0, bytesPerDay)
}

func newBlobMeter(clk clock.Clock, perMinute int, perDay int64) *UploadMeter {
	return &UploadMeter{clk: clk, perMinute: float64(perMinute), perDay: float64(perDay),
		users: map[id.ID]*uploadBuckets{}}
}

// refill brings u up to now and answers it.
func (m *UploadMeter) refill(user id.ID) *uploadBuckets {
	now := m.clk.Now()
	u, ok := m.users[user]
	if !ok {
		u = &uploadBuckets{requests: m.perMinute, bytes: m.perDay, at: now}
		m.users[user] = u
		return u
	}
	elapsed := now.Sub(u.at).Seconds()
	if elapsed > 0 {
		u.requests = math.Min(m.perMinute, u.requests+elapsed*m.perMinute/60)
		u.bytes = math.Min(m.perDay, u.bytes+elapsed*m.perDay/86400)
		u.at = now
	}
	return u
}

// begin reserves one upload of at most reserve bytes for user, or answers 429 E_RATE_LIMITED with
// the wait until it would fit. It answers the byte reservation settle must be given back.
func (m *UploadMeter) begin(user id.ID, reserve int64) (int64, error) {
	if m == nil {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.refill(user)
	if m.perMinute > 0 && u.requests < 1 {
		return 0, server.RateLimitedAfter(time.Duration((1 - u.requests) * 60 / m.perMinute * float64(time.Second)))
	}
	held := float64(reserve)
	if m.perDay <= 0 {
		held = 0
	} else if held > m.perDay {
		// A reservation larger than the whole budget could never be met; the body
		// limit still bounds what one upload writes.
		held = m.perDay
	}
	if held > 0 && u.bytes < held {
		return 0, server.RateLimitedAfter(time.Duration((held - u.bytes) * 86400 / m.perDay * float64(time.Second)))
	}
	if m.perMinute > 0 {
		u.requests--
	}
	u.bytes -= held
	return int64(held), nil
}

// settle gives back what begin held beyond the bytes the upload actually read.
func (m *UploadMeter) settle(user id.ID, held, used int64) {
	if m == nil || m.perDay <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.refill(user)
	u.bytes = math.Min(m.perDay, u.bytes+float64(held-used))
}
