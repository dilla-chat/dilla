//go:build linux

package main

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// Task 3 (G3 repeat-start drift): a start's catch-up spends no read token on dillad's group head when the gateway's
// hints already said where it is. The hinted head is the group's next sequence after the newest hinted handshake or
// message, kept per gateway connection; a reconnect, a closed gateway or an old hint falls back to the read.

type headsRig struct {
	events       *musicEvents
	installation id.ID
	group        id.ID
	conn         uint64
	now          time.Time
	remoteReads  int
	remoteNext   uint64
}

func newHeadsRig() *headsRig {
	return &headsRig{events: &musicEvents{}, installation: id.New(), group: id.New(), conn: 7, now: time.Unix(1_800_000_000, 0), remoteNext: 9}
}

func (r *headsRig) catchUp(localNext uint64, sync *catchUpSyncFake) musicCallCatchUp {
	return musicCallCatchUp{sync: sync, session: &client.Session{},
		local: func(context.Context, id.ID) (native.GroupInfo, error) {
			return native.GroupInfo{NextSequence: localNext}, nil
		},
		remote: func(context.Context, id.ID) (client.GroupInfo, error) {
			r.remoteReads++
			return client.GroupInfo{NextSequence: r.remoteNext}, nil
		},
		heads: &musicGroupHeads{events: r.events, installation: r.installation,
			connection: func() uint64 { return r.conn }, now: func() time.Time { return r.now }},
	}
}

func TestCatchUpSkipsRemoteReadWhenHintIsCurrent(t *testing.T) {
	r := newHeadsRig()
	// The newest hint was sequence 8: the group's next sequence is 9, which the helper already holds.
	r.events.groupHinted(r.installation, r.group, 9, r.conn, r.now)
	sync := &catchUpSyncFake{}
	if err := r.catchUp(9, sync).run(t.Context(), r.group); err != nil || r.remoteReads != 0 || sync.calls != 0 {
		t.Fatalf("a hinted current group: err=%v GroupInfo calls=%d syncs=%d, want no read and no paging", err, r.remoteReads, sync.calls)
	}
	// A hint past the helper: the catch-up reads the head and pages.
	r.events.groupHinted(r.installation, r.group, 12, r.conn, r.now)
	r.remoteNext = 12
	sync = &catchUpSyncFake{}
	if err := r.catchUp(9, sync).run(t.Context(), r.group); err != nil || r.remoteReads != 1 || sync.calls != 1 {
		t.Fatalf("a hinted group ahead of the helper: err=%v GroupInfo calls=%d syncs=%d, want one read and one pass", err, r.remoteReads, sync.calls)
	}
}

func TestCatchUpReadsRemoteWithoutHint(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *headsRig)
	}{
		{"no hint seen yet", func(*headsRig) {}},
		{"the stream reconnected after the hint", func(r *headsRig) {
			r.events.groupHinted(r.installation, r.group, 9, r.conn, r.now)
			r.conn++
		}},
		{"the gateway has no connection", func(r *headsRig) {
			r.events.groupHinted(r.installation, r.group, 9, r.conn, r.now)
			r.conn = 0
		}},
		{"a hint of another group", func(r *headsRig) {
			r.events.groupHinted(r.installation, id.New(), 9, r.conn, r.now)
		}},
		{"a hint older than the head's max age", func(r *headsRig) {
			r.events.groupHinted(r.installation, r.group, 9, r.conn, r.now)
			r.now = r.now.Add(musicHintedHeadMaxAge + time.Millisecond)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newHeadsRig()
			tc.setup(r)
			sync := &catchUpSyncFake{}
			if err := r.catchUp(9, sync).run(t.Context(), r.group); err != nil || r.remoteReads != 1 || sync.calls != 0 {
				t.Fatalf("err=%v GroupInfo calls=%d syncs=%d, want one read and no paging", err, r.remoteReads, sync.calls)
			}
		})
	}
}

// A read that finds the helper current renews a hinted head of the same connection (the monitor tick keeps a quiet
// group's head fresh), but never makes one: only a hint on the live connection does.
func TestCatchUpReadRenewsOnlyAHintedHead(t *testing.T) {
	r := newHeadsRig()
	sync := &catchUpSyncFake{}
	if err := r.catchUp(9, sync).run(t.Context(), r.group); err != nil || r.remoteReads != 1 {
		t.Fatalf("first read: err=%v reads=%d", err, r.remoteReads)
	}
	if err := r.catchUp(9, sync).run(t.Context(), r.group); err != nil || r.remoteReads != 2 {
		t.Fatalf("a read made a head without a hint: reads=%d", r.remoteReads)
	}
	r.events.groupHinted(r.installation, r.group, 9, r.conn, r.now)
	r.now = r.now.Add(musicHintedHeadMaxAge + time.Millisecond)
	if err := r.catchUp(9, sync).run(t.Context(), r.group); err != nil || r.remoteReads != 3 {
		t.Fatalf("an old head skipped the read: reads=%d", r.remoteReads)
	}
	// That read renewed the head: the next start skips.
	if err := r.catchUp(9, sync).run(t.Context(), r.group); err != nil || r.remoteReads != 3 {
		t.Fatalf("a renewed head still read: reads=%d", r.remoteReads)
	}
}
