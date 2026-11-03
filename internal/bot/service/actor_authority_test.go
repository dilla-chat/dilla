package service

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

type actorAuthoritySessionFake struct {
	community, actor id.ID
	authority        client.ActorAuthority
	present          bool
	err              error
	calls            int
}

func (f *actorAuthoritySessionFake) BotActorAuthority(_ context.Context, community, actor id.ID) (client.ActorAuthority, bool, error) {
	f.calls++
	f.community, f.actor = community, actor
	return f.authority, f.present, f.err
}

func TestCommunityActorAuthorityUsesAuthenticatedCurrentLookup(t *testing.T) {
	community, actor := id.New(), id.New()
	session := &actorAuthoritySessionFake{authority: client.ActorAuthority{EffectiveBits: 1 << 16, Owner: false, Administrator: true}, present: true}
	resolver, err := NewCommunityActorAuthority(session, community, func(context.Context, id.ID) (uint64, error) { return 7, nil })
	if err != nil {
		t.Fatal(err)
	}
	event := native.GroupEvent{Community: community, SenderUser: actor}
	owner, admin, err := resolver.ModerationActorRoles(context.Background(), event)
	if err != nil || owner || !admin || session.community != community || session.actor != actor || session.calls != 1 {
		t.Fatalf("moderation authority=(%v,%v,%v), lookup=(%s,%s), calls=%d", owner, admin, err, session.community, session.actor, session.calls)
	}
	allowed, err := resolver.CanManageCommunity(context.Background(), actor, community)
	if err != nil || !allowed {
		t.Fatalf("CanManageCommunity=(%v,%v)", allowed, err)
	}
	allowed, err = resolver.CanManageCommunity(context.Background(), actor, id.New())
	if err == nil || allowed {
		t.Fatalf("foreign CanManageCommunity=(%v,%v), want refusal", allowed, err)
	}
	if got, err := resolver.CurrentRevision(context.Background(), community); err != nil || got != 7 {
		t.Fatalf("CurrentRevision=(%d,%v)", got, err)
	}
}

func TestCommunityActorAuthorityDoesNotElevateMissingOrMismatchedActor(t *testing.T) {
	community, actor := id.New(), id.New()
	session := &actorAuthoritySessionFake{authority: client.ActorAuthority{Owner: true, Administrator: true}, present: false}
	resolver, err := NewCommunityActorAuthority(session, community, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner, admin, err := resolver.ModerationActorRoles(context.Background(), native.GroupEvent{Community: community, SenderUser: actor})
	if err != nil || owner || admin {
		t.Fatalf("missing actor roles=(%v,%v,%v)", owner, admin, err)
	}
	owner, admin, err = resolver.ModerationActorRoles(context.Background(), native.GroupEvent{Community: id.New(), SenderUser: actor})
	if err == nil || owner || admin {
		t.Fatalf("mismatched community roles=(%v,%v,%v)", owner, admin, err)
	}
	if _, err := resolver.CurrentRevision(context.Background(), community); err == nil {
		t.Fatal("missing revision reader accepted")
	}
}
