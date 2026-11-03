package service

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

type countedCaseState struct {
	*moderationCaseNativeFake
	scans int
}

func (n *countedCaseState) StateScan(ctx context.Context, area uint64, after []byte, limit uint64) (native.StatePage, error) {
	n.scans++
	return n.moderationCaseNativeFake.StateScan(ctx, area, after, limit)
}

type caseReadAuthority struct {
	result    client.ActorAuthority
	present   bool
	err       error
	calls     int
	community id.ID
	actor     id.ID
}

func (a *caseReadAuthority) BotActorAuthority(_ context.Context, community, actor id.ID) (client.ActorAuthority, bool, error) {
	a.calls++
	a.community, a.actor = community, actor
	return a.result, a.present, a.err
}

func TestModerationCaseReaderChecksFreshViewAuditPermissionBeforeScanning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
		present bool
		err     error
	}{
		{name: "allowed", allowed: true, present: true},
		{name: "permission revoked", present: true},
		{name: "actor absent"},
		{name: "lookup unavailable", present: true, err: errors.New("unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &countedCaseState{moderationCaseNativeFake: newModerationCaseNative()}
			sink, err := NewModerationCaseSink(state, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			authority := &caseReadAuthority{present: tc.present, err: tc.err}
			if tc.allowed {
				authority.result.EffectiveBits = uint64(1 << 17)
			}
			reader, err := NewModerationCaseReader(sink, authority)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = reader.List(context.Background(), id.New(), nil, 10)
			if tc.allowed {
				if err != nil || state.scans != 1 {
					t.Fatalf("authorized read scans=%d err=%v", state.scans, err)
				}
			} else if !errors.Is(err, ErrModerationCaseUnauthorized) || state.scans != 0 {
				t.Fatalf("denied read scans=%d err=%v", state.scans, err)
			}
			if authority.calls != 1 {
				t.Fatalf("authority lookups=%d, want one fresh lookup", authority.calls)
			}
		})
	}
}

func TestModerationCaseReaderRechecksPermissionForEveryPage(t *testing.T) {
	state := &countedCaseState{moderationCaseNativeFake: newModerationCaseNative()}
	sink, err := NewModerationCaseSink(state, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	authority := &caseReadAuthority{result: client.ActorAuthority{EffectiveBits: uint64(1 << 17)}, present: true}
	reader, err := NewModerationCaseReader(sink, authority)
	if err != nil {
		t.Fatal(err)
	}
	actor := id.New()
	if _, _, err := reader.List(context.Background(), actor, nil, 10); err != nil {
		t.Fatal(err)
	}
	authority.result.EffectiveBits = 0
	if _, _, err := reader.List(context.Background(), actor, nil, 10); !errors.Is(err, ErrModerationCaseUnauthorized) {
		t.Fatalf("read after permission revocation = %v", err)
	}
	if authority.calls != 2 || state.scans != 1 {
		t.Fatalf("authorization was not refreshed: lookups=%d scans=%d", authority.calls, state.scans)
	}
}
