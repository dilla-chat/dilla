package service

import (
	"context"
	"log/slog"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// v0.1.0-bot2.2 (F6): a Bot channel join, done by applying the text group's Welcome, logged its intent and nothing
// else. It now logs the outcome: joined, or failed with the phase and the error class.
func TestWelcomeApplyLogsJoined(t *testing.T) {
	r, n, s, community, channel, group := membershipFixture(t)
	channels := &botChannelSet{known: true}
	channels.set(channel, true)
	logs := &recordedLogs{}
	r.UseBotChannels(channels, slog.New(logs))
	s.welcomes = []client.Welcome{{ID: 2, GroupID: group, Epoch: 5, CommitSequence: 9, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	n.outcomes = []native.GroupWelcomeOutcome{{ID: 2, Group: group, Outcome: 0}}
	n.inspected = native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}
	if _, err := r.reconcileOnce(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	joined := logs.with("bot joined Bot channel group")
	if len(joined) != 1 || joined[0]["channel_id"] != channel.String() {
		t.Fatalf("joined records %v, want one for channel %s", joined, channel)
	}
	if got := len(logs.with("bot Bot channel join failed")); got != 0 {
		t.Fatalf("%d join-failed records after a join", got)
	}
}

func TestWelcomeApplyFailureLogsJoinFailed(t *testing.T) {
	r, n, s, _, channel, group := membershipFixture(t)
	channels := &botChannelSet{known: true}
	channels.set(channel, true)
	logs := &recordedLogs{}
	r.UseBotChannels(channels, slog.New(logs))
	s.welcomes = []client.Welcome{{ID: 2, GroupID: group, Epoch: 5, CommitSequence: 9, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
	n.applyErr = helper.ErrClosed
	if _, err := r.reconcileOnce(context.Background(), s); err == nil {
		t.Fatal("a failed apply returned no error")
	}
	failed := logs.with("bot Bot channel join failed")
	if len(failed) != 1 || failed[0]["channel_id"] != channel.String() || failed[0]["phase"] != "welcome_apply" || failed[0]["error_class"] != helper.Class(helper.ErrClosed) {
		t.Fatalf("join-failed records %v, want channel %s, phase welcome_apply and the error's class", failed, channel)
	}
	if got := len(logs.with("bot joined Bot channel group")); got != 0 {
		t.Fatalf("%d joined records after a failed apply", got)
	}
}

// Each later step of the join logs its own phase.
func TestJoinFailedLogsEachLaterPhase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phase  string
		break_ func(n *membershipFakeNative, s *membershipFakeSession, group id.ID)
	}{
		{"outcome_validation", "outcome_validation", func(n *membershipFakeNative, _ *membershipFakeSession, group id.ID) {
			n.outcomes = []native.GroupWelcomeOutcome{{ID: 99, Group: group, Outcome: 0}}
		}},
		{"welcome_revalidation", "welcome_revalidation", func(_ *membershipFakeNative, s *membershipFakeSession, _ id.ID) { s.changeGroupOnSecond = true }},
		{"welcome_acknowledge", "welcome_acknowledge", func(_ *membershipFakeNative, s *membershipFakeSession, _ id.ID) { s.ackErr = helper.ErrClosed }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, n, s, community, channel, group := membershipFixture(t)
			channels := &botChannelSet{known: true}
			channels.set(channel, true)
			logs := &recordedLogs{}
			r.UseBotChannels(channels, slog.New(logs))
			s.welcomes = []client.Welcome{{ID: 2, GroupID: group, Epoch: 5, CommitSequence: 9, Blob: []byte{1}, Tree: []byte{2}, TreeHash: make([]byte, 32)}}
			n.outcomes = []native.GroupWelcomeOutcome{{ID: 2, Group: group, Outcome: 0}}
			n.inspected = native.GroupInfo{ID: group, Kind: 0, Community: &community, Target: channel, State: 2, NextSequence: 1}
			tc.break_(n, s, group)
			if _, err := r.reconcileOnce(context.Background(), s); err == nil {
				t.Fatal("the broken join returned no error")
			}
			failed := logs.with("bot Bot channel join failed")
			if len(failed) != 1 || failed[0]["phase"] != tc.phase || failed[0]["channel_id"] != channel.String() {
				t.Fatalf("join-failed records %v, want one with phase %s", failed, tc.phase)
			}
			if got := len(logs.with("bot joined Bot channel group")); got != 0 {
				t.Fatalf("%d joined records", got)
			}
		})
	}
}
