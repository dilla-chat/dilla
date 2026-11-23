package sfu

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/livekit/protocol/logger"
)

// The bridge's bounds (branch review SFU-2 and parked item 2). A participant's signalling socket
// takes messages up to 2 MiB, LiveKit logs a bad offer whole at INFO under logr, and logr cannot
// sample, so the bridge is where a member's input stops.
const (
	// maxBridgedLine is the most one bridged line takes in dillad's log, with its time and level.
	maxBridgedLine = 4096
	// maxMessage, maxValue and maxError cap a record's message, an allowed attribute's string value
	// and an error's text.
	maxMessage = 256
	maxValue   = 64
	maxError   = 256
	// maxAttrs is the most attributes one record, or one WithAttrs, carries on.
	maxAttrs = 16
	// bridgeRate and bridgeBurst are the token bucket per (level, logger, message): 5 lines a second
	// with a burst of 20. What the bucket refuses is counted and reported once per
	// suppressionInterval as one "LiveKit log lines suppressed" line per bucket.
	bridgeRate          = 5
	bridgeBurst         = 20
	suppressionInterval = 10 * time.Second
	// maxBuckets bounds the bucket map. A new key on a full map evicts an idle bucket (refilled,
	// nothing suppressed); when none is idle it shares its level's overflow bucket, so an ERROR never
	// waits behind INFO lines.
	maxBuckets = 512
)

// NewLogBridge is the slog handler LiveKit's and pion's logs go through (b.8). logr names arrive as
// one "logger" attribute joined with "/" (livekit/transport/pion.ice); pion's components no longer
// honour logging.pion_level, so their records below ERROR are dropped here.
//
// LiveKit's own redaction lives in zap MarshalLogObject methods, which slog never calls: a slog
// handler json.Marshals a bound *routing.ParticipantInit whole, with the client's address, its
// grants and metadata and a v1 publisher offer. So the bridge forwards only the attributes it names
// (allowedAttrs and renamed), drops every other key and every group, masks IP addresses in what it
// keeps, caps every value and the message, and rate-limits each (level, logger, message). The
// participant identity — a dilla device id — becomes device_id, LiveKit's session id
// participant_id, and the room keeps its call id's first eight characters, all shortened like every
// *_id.
func NewLogBridge(next slog.Handler) slog.Handler {
	return newLogBridge(next, time.Now)
}

func newLogBridge(next slog.Handler, now func() time.Time) slog.Handler {
	if next == nil {
		next = slog.DiscardHandler
	}
	return logBridge{next: next, limits: &bridgeLimits{root: next, now: now, buckets: map[string]*bucket{},
		overflow: map[slog.Level]*bucket{}}}
}

type logBridge struct {
	next   slog.Handler
	limits *bridgeLimits
}

func (b logBridge) Enabled(ctx context.Context, l slog.Level) bool { return b.next.Enabled(ctx, l) }

func (b logBridge) Handle(ctx context.Context, r slog.Record) error {
	name := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			name = a.Value.String()
			return false
		}
		return true
	})
	if strings.Contains(name, "/pion.") && r.Level < slog.LevelError {
		return nil
	}
	msg := capString(maskAddrs(r.Message), maxMessage)
	allowed, summaries := b.limits.take(r.Level, name, msg)
	for _, s := range summaries {
		_ = b.limits.root.Handle(ctx, s)
	}
	if !allowed {
		return nil
	}
	out := slog.NewRecord(r.Time, r.Level, msg, r.PC)
	n := 0
	r.Attrs(func(a slog.Attr) bool {
		if kept, ok := allow(a); ok {
			out.AddAttrs(kept)
			n++
		}
		return n < maxAttrs
	})
	return b.next.Handle(ctx, out)
}

func (b logBridge) WithAttrs(as []slog.Attr) slog.Handler {
	kept := make([]slog.Attr, 0, min(len(as), maxAttrs))
	for _, a := range as {
		if k, ok := allow(a); ok && len(kept) < maxAttrs {
			kept = append(kept, k)
		}
	}
	return logBridge{next: b.next.WithAttrs(kept), limits: b.limits}
}

func (b logBridge) WithGroup(name string) slog.Handler {
	return logBridge{next: b.next.WithGroup(name), limits: b.limits}
}

// renamed maps a LiveKit key to the key dillad logs it under, shortened as an id.
var renamed = map[string]string{
	"participant":   "device_id",
	"participantID": "participant_id",
	"pID":           "participant_id",
	"roomID":        "room_id",
	"trackID":       "track_id",
}

// allowedAttrs are the LiveKit keys forwarded under their own name: scalars LiveKit uses for joins,
// leaves, tracks and transports. Their values are scalars, enum names or short strings; anything
// else (a struct, a map, a slice, a group) is dropped.
var allowedAttrs = map[string]bool{
	"logger": true, "kind": true, "mime": true, "mimeType": true, "source": true, "transport": true,
	"state": true, "reason": true, "status": true, "method": true, "path": true, "numParticipants": true,
	"nodeID": true, "relayed": true, "layer": true, "codec": true, "direction": true, "attempt": true,
	"code": true, "connectionType": true, "isMigration": true, "isResume": true,
}

// allow is the attribute dillad's log receives for a, and false when it receives none.
func allow(a slog.Attr) (slog.Attr, bool) {
	a.Value = a.Value.Resolve()
	switch {
	case a.Key == "error" || a.Key == "err":
		return slog.String("error", capString(maskAddrs(text(a.Value)), maxError)), true
	case a.Key == "room":
		return slog.String("room", shortRoom(text(a.Value))), true
	case renamed[a.Key] != "":
		return slog.String(renamed[a.Key], short(a.Value)), true
	case !allowedAttrs[a.Key]:
		return slog.Attr{}, false
	}
	switch a.Value.Kind() {
	case slog.KindBool, slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindDuration, slog.KindTime:
		return a, true
	case slog.KindString:
		return slog.String(a.Key, capString(maskAddrs(a.Value.String()), maxValue)), true
	case slog.KindAny:
		v := a.Value.Any()
		if s, ok := v.(fmt.Stringer); ok {
			return slog.String(a.Key, capString(maskAddrs(s.String()), maxValue)), true
		}
		switch reflect.ValueOf(v).Kind() {
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64, reflect.String:
			return slog.String(a.Key, capString(maskAddrs(fmt.Sprint(v)), maxValue)), true
		default:
		}
	default:
	}
	return slog.Attr{}, false
}

// text is v as a string: an error's or a Stringer's text, a string as it is.
func text(v slog.Value) string {
	if v.Kind() == slog.KindAny {
		switch x := v.Any().(type) {
		case error:
			return x.Error()
		case fmt.Stringer:
			return x.String()
		}
	}
	return v.String()
}

// addrPattern is an IPv4 literal, an IPv6 one (in brackets; with an embedded IPv4 such as the
// IPv4-mapped ::ffff:203.0.113.7 netip prints; with "::", leading groups included; or with at least
// five groups), each with an optional %zone and port. A pion ICE or DTLS error embeds both sides'
// addresses. The embedded-IPv4 form comes before plain IPv4, so no octet survives.
var addrPattern = regexp.MustCompile(`\[[0-9a-fA-F:.%\w]*:[0-9a-fA-F:.%\w]*\](?::\d{1,5})?` +
	`|(?:[0-9a-fA-F]{0,4}:){2,6}(?:\d{1,3}\.){3}\d{1,3}(?:%[\w.]+)?(?::\d{1,5})?` +
	`|(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?` +
	`|(?:[0-9a-fA-F]{1,4}(?::[0-9a-fA-F]{1,4})*)?::(?:[0-9a-fA-F]{1,4}(?::[0-9a-fA-F]{1,4})*)?(?:%[\w.]+)?` +
	`|(?:[0-9a-fA-F]{1,4}:){4,7}[0-9a-fA-F]{1,4}(?:%[\w.]+)?`)

// maskAddrs replaces every address in s with [addr].
func maskAddrs(s string) string {
	if !strings.ContainsAny(s, ".:") {
		return s
	}
	return addrPattern.ReplaceAllString(s, "[addr]")
}

// capString cuts s to at most n bytes, on a rune boundary, marking the cut.
func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// short is the first eight characters of an identifier, as dillad's redactor logs every *_id.
func short(v slog.Value) string {
	s := text(v)
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// shortRoom is a LiveKit room name "<call id>-<unix seconds>-<random>" with the call id cut to its
// first eight characters (SFU-9): enough to correlate lines, not the call id. What follows the first
// "-" is kept, capped. A room already short is unchanged.
func shortRoom(room string) string {
	call, rest, found := strings.Cut(room, "-")
	if len(call) > 8 {
		call = call[:8]
	}
	if !found {
		return call
	}
	return call + "-" + capString(maskAddrs(rest), maxValue)
}

// bridgeLimits is the bridge's rate limiter, shared by every handler derived from one NewLogBridge.
// Its summaries go to root, the handler NewLogBridge was given, without any logger's bound attrs.
type bridgeLimits struct {
	root    slog.Handler
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*bucket
	// overflow is one bucket per level for the lines whose own bucket would not fit.
	overflow  map[slog.Level]*bucket
	lastFlush time.Time
}

type bucket struct {
	tokens      float64
	last        time.Time
	suppressed  int
	level       slog.Level
	logger, msg string
}

// take spends a token of the (level, logger, msg) bucket and answers whether the line may be
// written, with the suppression summaries that are due.
func (l *bridgeLimits) take(level slog.Level, name, msg string) (bool, []slog.Record) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastFlush.IsZero() {
		l.lastFlush = now
	}
	key := level.String() + "\x00" + name + "\x00" + msg
	b := l.buckets[key]
	if b == nil && len(l.buckets) >= maxBuckets && !l.evictIdle(now) {
		// No bucket is idle: the line shares its level's overflow bucket, so a new ERROR is never
		// held by the overflow of lower levels (re-review N2).
		if b = l.overflow[level]; b == nil {
			b = &bucket{tokens: bridgeBurst, last: now, level: level, msg: "(other messages)"}
			l.overflow[level] = b
		}
	}
	if b == nil {
		b = &bucket{tokens: bridgeBurst, last: now, level: level, logger: name, msg: msg}
		l.buckets[key] = b
	}
	b.tokens = min(bridgeBurst, b.tokens+now.Sub(b.last).Seconds()*bridgeRate)
	b.last = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	} else {
		b.suppressed++
	}
	var due []slog.Record
	if now.Sub(l.lastFlush) >= suppressionInterval {
		l.lastFlush = now
		summarise := func(s *bucket) {
			if s.suppressed == 0 {
				return
			}
			r := slog.NewRecord(now, slog.LevelWarn, "LiveKit log lines suppressed", 0)
			r.AddAttrs(slog.String("logger", s.logger), slog.String("suppressed_msg", s.msg),
				slog.String("suppressed_level", s.level.String()), slog.Int("suppressed", s.suppressed),
				slog.Duration("interval", suppressionInterval))
			due = append(due, r)
			s.suppressed = 0
		}
		for _, s := range l.buckets {
			summarise(s)
		}
		for _, s := range l.overflow {
			summarise(s)
		}
	}
	return allowed, due
}

// evictIdle deletes one bucket that has refilled to its burst and suppressed nothing since its last
// summary, and answers whether it found one: such a bucket holds no state worth keeping.
func (l *bridgeLimits) evictIdle(now time.Time) bool {
	for k, b := range l.buckets {
		if b.suppressed == 0 && b.tokens+now.Sub(b.last).Seconds()*bridgeRate >= bridgeBurst {
			delete(l.buckets, k)
			return true
		}
	}
	return false
}

// liveKitSink is the handler the one process-wide LiveKit logger writes through. LiveKit's
// logger.SetLogger assigns two package-level variables without synchronisation, and goroutines left
// over from an earlier in-process server (telemetry queues) read them while they log, so it runs once.
// A later Start swaps the handler here instead.
var (
	liveKitSink atomic.Pointer[slog.Handler]
	liveKitOnce sync.Once
	discardSink = slog.DiscardHandler
)

// init installs the logger before any goroutine can read it. LiveKit starts such goroutines outside
// Start (prometheus.Init's hwstats sampler logs "high cpu load" while the host is busy), so a first
// install made later, from Start, would write the package-level logger under a running reader.
// Until a Start supplies a handler the bridge discards.
func init() { installLiveKitLogger(nil) }

// installLiveKitLogger points LiveKit's and pion's logs at next. The first call, at package init,
// installs the logger; every later call only swaps the handler, so LiveKit's package-level logger is
// written exactly once, before any reader exists.
func installLiveKitLogger(next slog.Handler) {
	if next == nil {
		next = slog.DiscardHandler
	}
	liveKitSink.Store(&next)
	liveKitOnce.Do(func() {
		logger.SetLogger(pionGated{logger.LogRLogger(logr.FromSlogHandler(NewLogBridge(&swapHandler{})))}, "livekit")
	})
}

// swapHandler forwards to whatever handler liveKitSink holds at the moment of the call. The attrs and
// groups a logger derived from it carries are replayed onto that handler once per swap and cached
// (SFU-6), so a derived logger created under one Start follows the next Start's handler too, and no
// record re-encodes them.
type swapHandler struct {
	ops   []func(slog.Handler) slog.Handler
	cache atomic.Pointer[derivedSink]
}

// derivedSink is ops replayed onto the sink base points at; a swap stores a new pointer.
type derivedSink struct {
	base *slog.Handler
	h    slog.Handler
}

func currentSink() *slog.Handler {
	if p := liveKitSink.Load(); p != nil {
		return p
	}
	return &discardSink
}

func (h *swapHandler) current() slog.Handler {
	base := currentSink()
	if c := h.cache.Load(); c != nil && c.base == base {
		return c.h
	}
	cur := *base
	for _, op := range h.ops {
		cur = op(cur)
	}
	h.cache.Store(&derivedSink{base: base, h: cur})
	return cur
}

// Enabled asks the sink alone: WithAttrs and WithGroup never change a level decision of dillad's
// handlers, and logr asks before every record, enabled or not.
func (h *swapHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return (*currentSink()).Enabled(ctx, l)
}

func (h *swapHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.current().Handle(ctx, r)
}

func (h *swapHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &swapHandler{ops: append(slices.Clone(h.ops), func(n slog.Handler) slog.Handler { return n.WithAttrs(as) })}
}

func (h *swapHandler) WithGroup(name string) slog.Handler {
	return &swapHandler{ops: append(slices.Clone(h.ops), func(n slog.Handler) slog.Handler { return n.WithGroup(name) })}
}
