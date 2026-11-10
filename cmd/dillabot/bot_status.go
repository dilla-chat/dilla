//go:build linux

package main

import (
	"slices"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 22: the Bot page's status read (sealed operation 94). It answers, to members who manage the community,
// the Bot channels whose group the Bot cannot join on its own (the membership reconciler's NeedsReAdd, kept by the
// installation's feed) and the last music starts. Nothing here reads dillad: both are the Bot's own state.

// botStatusSource is the status read's source for one installation: its feed's needs-re-add set and its music starts.
func (r *botGatewayRuntime) botStatusSource(installation id.ID, starts func() []music.StartTiming) func() service.BotStatus {
	return func() service.BotStatus {
		var out service.BotStatus
		if f := r.feedHandler(installation); f != nil {
			for channel, needed := range f.NeedsReAdd() {
				if needed {
					out.NeedsReAdd = append(out.NeedsReAdd, channel)
				}
			}
		}
		if starts != nil {
			for _, t := range starts() {
				out.MusicStarts = append(out.MusicStarts, service.MusicStart{FirstFrame: t.FirstFrame, Cold: t.Cold})
			}
		}
		return out
	}
}

// StartTimings is the installation's last 20 track starts across its voice rooms, oldest first.
func (r *musicQueueRegistry) StartTimings(installation, community id.ID) []music.StartTiming {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	retained := r.runtimes[installation]
	var queues *musicQueueRuntime
	if retained != nil && retained.community == community {
		queues = retained.queues
	}
	r.mu.Unlock()
	if queues == nil {
		return nil
	}
	queues.mu.Lock()
	supervisors := make([]*music.Supervisor, 0, len(queues.supervisors))
	for _, supervisor := range queues.supervisors {
		supervisors = append(supervisors, supervisor)
	}
	queues.mu.Unlock()
	var all []music.StartTiming
	for _, supervisor := range supervisors {
		all = append(all, supervisor.Timings()...)
	}
	slices.SortStableFunc(all, func(a, b music.StartTiming) int { return a.At.Compare(b.At) })
	if len(all) > 20 {
		all = all[len(all)-20:]
	}
	return all
}
