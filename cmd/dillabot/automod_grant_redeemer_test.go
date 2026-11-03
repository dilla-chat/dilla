//go:build linux

package main

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

type autoModerationGrantSessionFake struct {
	installation, community, group, request, actorUser, actorDevice id.ID
	token                                                           string
	revision, expires                                               uint64
	hash                                                            [32]byte
	accepted                                                        bool
	receipt                                                         id.ID
	calls                                                           int
}

func (s *autoModerationGrantSessionFake) RedeemAutoModerationGrant(_ context.Context, installation id.ID, token string, community, group, request id.ID, revision, expires uint64, actorUser, actorDevice id.ID, configHash [32]byte) (bool, id.ID, error) {
	s.calls++
	s.installation, s.token, s.community, s.group, s.request = installation, token, community, group, request
	s.revision, s.expires, s.actorUser, s.actorDevice, s.hash = revision, expires, actorUser, actorDevice, configHash
	return s.accepted, s.receipt, nil
}

func TestAutoModerationGrantRedeemerSendsOnlyExactDigestBinding(t *testing.T) {
	session := &autoModerationGrantSessionFake{accepted: true, receipt: id.New()}
	installation, community, group, request := id.New(), id.New(), id.New(), id.New()
	actorUser, actorDevice := id.New(), id.New()
	hash := [32]byte{0x44}
	redeemer := autoModerationGrantRedeemer{Session: session, InstallationID: installation}
	accepted, err := redeemer.RedeemAutoModerationGrant(context.Background(), "opaque-grant", service.AutoModerationGrantBinding{
		RequestID: request, CommunityID: community, ActorUser: actorUser, ActorDevice: actorDevice,
		Group: group, Operation: protocol.OpAutoModerationPut, ExpectedRevision: 0, RequestExpires: 120, ConfigHash: hash,
	})
	if err != nil || !accepted || session.calls != 1 || session.installation != installation || session.token != "opaque-grant" || session.community != community || session.group != group || session.request != request || session.actorUser != actorUser || session.actorDevice != actorDevice || session.revision != 0 || session.expires != 120 || session.hash != hash {
		t.Fatalf("grant redemption accepted=%v calls=%d session=%+v err=%v", accepted, session.calls, session, err)
	}
}

func TestAutoModerationGrantRedeemerRejectsInvalidBindingBeforeHTTP(t *testing.T) {
	session := &autoModerationGrantSessionFake{accepted: true, receipt: id.New()}
	redeemer := autoModerationGrantRedeemer{Session: session, InstallationID: id.New()}
	accepted, err := redeemer.RedeemAutoModerationGrant(context.Background(), "opaque-grant", service.AutoModerationGrantBinding{Operation: protocol.OpAutoModerationPut})
	if err == nil || accepted || session.calls != 0 {
		t.Fatalf("invalid binding accepted=%v calls=%d err=%v", accepted, session.calls, err)
	}
}
