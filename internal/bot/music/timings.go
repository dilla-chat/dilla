package music

import (
	"context"
	"sort"
	"sync"
	"time"
)

// StartTiming is what one track start cost, by stage. Resolve is the lookup
// (zero when pre-resolved), FirstByte the wait until the stream delivered
// its first byte, FirstFrame the time from the start request to the first
// audio frame being published. Cold is true when the lookup ran yt-dlp.
type StartTiming struct {
	Resolve, FirstByte, FirstFrame time.Duration
	At                             time.Time
	Track                          string
	Cold                           bool
}

// timingsKept is how many start timings the Bot shows.
const timingsKept = 20

type startTimerKey struct{}

// startTimer collects the stage marks providers report while one start runs.
type startTimer struct {
	mu                             sync.Mutex
	resolve, firstByte, firstFrame time.Duration
	haveResolve, haveByte, haveFrm bool
	cold                           bool
}

func withStartTimer(ctx context.Context) (context.Context, *startTimer) {
	timer := &startTimer{}
	return context.WithValue(ctx, startTimerKey{}, timer), timer
}

func startTimerFrom(ctx context.Context) *startTimer {
	timer, _ := ctx.Value(startTimerKey{}).(*startTimer)
	return timer
}

// MarkResolve reports the lookup stage of the start running under ctx.
// cold is true when yt-dlp ran. It is a no-op outside a supervised start.
func MarkResolve(ctx context.Context, d time.Duration, cold bool) {
	if timer := startTimerFrom(ctx); timer != nil {
		timer.mu.Lock()
		defer timer.mu.Unlock()
		if !timer.haveResolve {
			timer.resolve, timer.haveResolve = d, true
		}
		timer.cold = timer.cold || cold
	}
}

// MarkFirstByte reports the time until the stream's first byte.
func MarkFirstByte(ctx context.Context, d time.Duration) {
	if timer := startTimerFrom(ctx); timer != nil {
		timer.mu.Lock()
		defer timer.mu.Unlock()
		if !timer.haveByte {
			timer.firstByte, timer.haveByte = d, true
		}
	}
}

// MarkFirstFrame reports the time until the first audio frame was published.
func MarkFirstFrame(ctx context.Context, d time.Duration) {
	if timer := startTimerFrom(ctx); timer != nil {
		timer.mu.Lock()
		defer timer.mu.Unlock()
		if !timer.haveFrm {
			timer.firstFrame, timer.haveFrm = d, true
		}
	}
}

// finish turns the marks into one StartTiming. A start whose provider marked
// no first frame is measured whole: from the request to its return.
func (t *startTimer) finish(track string, began, now time.Time) StartTiming {
	t.mu.Lock()
	defer t.mu.Unlock()
	timing := StartTiming{Resolve: t.resolve, FirstByte: t.firstByte, FirstFrame: t.firstFrame, At: now, Track: track, Cold: t.cold}
	if !t.haveFrm {
		timing.FirstFrame = max(now.Sub(began), time.Nanosecond)
	}
	return timing
}

// Timings returns the last 20 track starts, oldest first.
func (s *Supervisor) Timings() []StartTiming {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StartTiming(nil), s.timings...)
}

func (s *Supervisor) recordTimingLocked(timing StartTiming) {
	s.timings = append(s.timings, timing)
	if len(s.timings) > timingsKept {
		s.timings = append([]StartTiming(nil), s.timings[len(s.timings)-timingsKept:]...)
	}
}

// StartP95 is the 95th percentile (nearest rank) of the first-frame time over
// timings; it feeds the health report's music_start_p95_ms. Zero without data.
func StartP95(timings []StartTiming) time.Duration {
	if len(timings) == 0 {
		return 0
	}
	frames := make([]time.Duration, len(timings))
	for i, timing := range timings {
		frames[i] = timing.FirstFrame
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i] < frames[j] })
	rank := (len(frames)*95 + 99) / 100
	return frames[rank-1]
}
