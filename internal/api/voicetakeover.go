package api

import (
	"context"

	botaccess "github.com/dilla-chat/dilla/internal/bot/access"
	"github.com/dilla-chat/dilla/internal/id"
)

// Voice takeover (bot2.2 task 8, Jonas 2026-11-11: "the last joined client should take over voice -
// just like discord"): a user is in voice on one device at a time. When LiveKit reports a device of
// the user joined a call room (participant_joined, so the new device is connected), every other
// device of the same user that a call room lists is removed from it, with pending reason 3
// (afkPendingTakenOver) recorded before the removal, so the cut client's first read says why and its
// recovery does not rejoin. The removed device's MLS leave follows through participant_left, as for
// any leave.
//
// The newest join wins, by the admission number (afkAdmitSeq) of each device's latest fresh /rtc
// admission (voiceFresh; a signal resume is the same session and no newer join): of the joining
// device and the user's other devices the rooms list, the one admitted last keeps voice and every
// other is cut. So a late participant_joined of an older admission cuts that device itself and no
// newer one, and a device admitted but not yet connected cuts nothing until its own join. Managed
// Bots and "#" shadow identities never take over and are never cut by one.

// takeoverCut is one device's takeover removal from one room; takeoverRetries maps each the SFU
// refused to the maintenance passes that retried it so far.
type takeoverCut struct {
	dev  id.ID
	room string
}

// noteVoiceFreshLocked records a fresh admission of dev of user under the admission number just
// taken (noteAFKAdmissionLocked). The caller holds afkMu.
func (h *Calls) noteVoiceFreshLocked(user, dev id.ID) {
	if h.voiceFresh == nil {
		h.voiceFresh = make(map[id.ID]map[id.ID]afkAdmit)
	}
	if h.voiceFresh[user] == nil {
		h.voiceFresh[user] = make(map[id.ID]afkAdmit)
	}
	h.voiceFresh[user][dev] = afkAdmit{seq: h.afkAdmitSeq, at: h.clk.Now()}
	// The device joins again: a takeover cut of its earlier session no longer applies.
	for c := range h.takeoverRetries {
		if c.dev == dev {
			delete(h.takeoverRetries, c)
		}
	}
}

// takeOverVoice runs the takeover for joined, a device of user LiveKit reports joined to room. The
// SFU is listed and called with no lock held; afkMu is held only to pick the winner and record the
// pending reasons, as cutUserEverywhere does.
func (h *Calls) takeOverVoice(ctx context.Context, user, joined id.ID, room string) {
	if h.sfu == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	bot, err := botaccess.IsBotUser(ctx, h.repo, user)
	if err != nil {
		h.log.WarnContext(ctx, "reading a joined device's user for voice takeover failed; nothing is taken over",
			"user", user, "device", joined, "err", err)
		return
	}
	if bot {
		return
	}
	devices, err := h.repo.ListDevicesByUser(ctx, user)
	if err != nil {
		h.log.WarnContext(ctx, "listing a user's devices for voice takeover failed; nothing is taken over",
			"user", user, "device", joined, "err", err)
		return
	}
	others := make(map[string]bool, len(devices))
	for _, d := range devices {
		if d.ID != joined {
			others[d.ID.String()] = true
		}
	}
	if len(others) == 0 {
		return
	}
	var present []takeoverCut
	seen := map[takeoverCut]bool{}
	if err := h.eachUserParticipant(ctx, others, func(r, d string) bool {
		dev, err := id.Parse(d)
		if err != nil {
			return true
		}
		if c := (takeoverCut{dev: dev, room: r}); !seen[c] {
			seen[c] = true
			present = append(present, c)
		}
		return true
	}); err != nil {
		h.log.WarnContext(ctx, "listing the SFU rooms for voice takeover failed; only the devices it listed are compared",
			"user", user, "device", joined, "err", err)
	}
	if len(present) == 0 {
		return
	}
	h.afkMu.Lock()
	fresh := h.voiceFresh[user]
	winner, best := joined, fresh[joined].seq
	for _, p := range present {
		if s := fresh[p.dev].seq; s > best {
			winner, best = p.dev, s
		}
	}
	if best == 0 {
		// No fresh admission of any of them is remembered: no join to call the newest.
		h.afkMu.Unlock()
		return
	}
	var cuts []takeoverCut
	for _, p := range present {
		if p.dev != winner {
			cuts = append(cuts, p)
		}
	}
	if winner != joined {
		cuts = append(cuts, takeoverCut{dev: joined, room: room})
	}
	for _, c := range cuts {
		h.setAFKPendingLocked(c.dev, afkPendingTakenOver)
	}
	h.afkMu.Unlock()
	for _, c := range cuts {
		done, err := h.takeoverRemove(ctx, c)
		if !done {
			continue
		}
		if err == nil {
			h.log.InfoContext(ctx, "voice moved to the user's newest device; an older one left its call",
				"user", user, "device", winner, "cut_device", c.dev, "room", c.room)
			continue
		}
		h.log.ErrorContext(ctx, "removing a device whose voice moved to a newer one failed; it keeps its pending reason, and maintenance retries the cut",
			"user", user, "cut_device", c.dev, "room", c.room, "err", err)
		h.afkMu.Lock()
		if h.takeoverRetries == nil {
			h.takeoverRetries = make(map[takeoverCut]int)
		}
		h.takeoverRetries[c] = 0
		h.afkMu.Unlock()
	}
}

// takeoverRemove removes c's device, with its "#" shadows, from c's room, and on success revokes its
// relay and stops its AFK timer. It answers false, removing nothing, when the device no longer holds
// pending 3: an admission since cleared it, and its new session is the newer join. Otherwise it
// answers true and the SFU's refusal, or nil.
func (h *Calls) takeoverRemove(ctx context.Context, c takeoverCut) (bool, error) {
	h.afkMu.Lock()
	p, ok := h.afkPending[c.dev]
	h.afkMu.Unlock()
	if !ok || p.reason != afkPendingTakenOver {
		return false, nil
	}
	rctx, cancel := sfuCtx(ctx)
	err := h.sfu.RemoveParticipants(rctx, c.room, c.dev)
	cancel()
	if err != nil {
		return true, err
	}
	h.revokeRelay(c.dev)
	h.afkMu.Lock()
	delete(h.afkTracked, c.dev)
	h.afkMu.Unlock()
	return true, nil
}

// retryTakeovers is AFKMaintenance's retry of the takeover cuts the SFU refused: a device still
// pending 3 is removed again; one admitted since (its pending cleared, or the retry dropped by its
// fresh admission) is left alone. A cut is given up after afkCutRetryMax failed passes.
func (h *Calls) retryTakeovers(ctx context.Context) {
	h.afkMu.Lock()
	due := make(map[takeoverCut]int, len(h.takeoverRetries))
	for c, n := range h.takeoverRetries {
		due[c] = n
	}
	h.afkMu.Unlock()
	for c, n := range due {
		if ctx.Err() != nil || h.draining() {
			return
		}
		done, err := h.takeoverRemove(ctx, c)
		h.afkMu.Lock()
		cur, queued := h.takeoverRetries[c]
		switch {
		case !queued || cur != n:
			// Dropped or replaced meanwhile (a fresh admission, a newer takeover): left as it is.
		case !done:
			delete(h.takeoverRetries, c)
		case err == nil:
			delete(h.takeoverRetries, c)
			h.log.InfoContext(ctx, "retried a voice takeover removal; it went through", "device", c.dev, "room", c.room, "attempt", n+1)
		case n+1 >= afkCutRetryMax:
			delete(h.takeoverRetries, c)
			h.log.ErrorContext(ctx, "a voice takeover removal failed on every retry; giving up, the older device may stay in its call",
				"device", c.dev, "room", c.room, "attempts", n+1, "err", err)
		default:
			h.takeoverRetries[c] = n + 1
			h.log.WarnContext(ctx, "retrying a voice takeover removal failed; the next maintenance pass tries again",
				"device", c.dev, "room", c.room, "attempt", n+1, "err", err)
		}
		h.afkMu.Unlock()
	}
}
