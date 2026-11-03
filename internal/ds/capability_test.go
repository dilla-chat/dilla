package ds_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

type capabilityRepo struct {
	store.Repository
	group   store.GroupRow
	members []store.MemberRow
}

func (r *capabilityRepo) GetGroup(context.Context, id.ID) (store.GroupRow, error) {
	return r.group, nil
}

func (r *capabilityRepo) ListMembers(context.Context, id.ID) ([]store.MemberRow, error) {
	return r.members, nil
}

func (r *capabilityRepo) GetDevice(_ context.Context, deviceID id.ID) (store.DeviceRow, error) {
	return store.DeviceRow{ID: deviceID}, nil
}

type capabilityACL bool

func (a capabilityACL) Eligible(context.Context, id.ID, id.ID) (bool, error) { return bool(a), nil }

func TestInteractionGroupGateRunsAfterMemberOrReaderAuthorization(t *testing.T) {
	ctx := context.Background()
	groupID, userID, deviceID := id.New(), id.New(), id.New()
	repo := &capabilityRepo{
		group:   store.GroupRow{GroupID: groupID, Kind: 3},
		members: []store.MemberRow{{GroupID: groupID, UserID: userID, DeviceID: deviceID}},
	}
	for _, tc := range []struct {
		name    string
		members []store.MemberRow
		acl     capabilityACL
		wire    uint64
		want    string
	}{
		{name: "member", members: repo.members, acl: false, wire: 2, want: "E_VERSION"},
		{name: "reader fallback", members: nil, acl: true, wire: 2, want: "E_VERSION"},
		{name: "unauthorized stays hidden", members: nil, acl: false, wire: 2, want: "E_NOT_FOUND"},
		{name: "wire three member", members: []store.MemberRow{{GroupID: groupID, UserID: userID, DeviceID: deviceID}}, wire: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo.members = tc.members
			service, err := ds.New(ds.Options{Store: repo, BotCapability: capability.New(nil), ACL: tc.acl})
			if err != nil {
				t.Fatalf("ds.New: %v", err)
			}
			defer service.Shutdown(ctx)
			session := auth.Session{Scope: auth.ScopeEnrolled, UserID: userID, DeviceID: deviceID, WireVersion: tc.wire}
			_, err = service.Info(ctx, groupID, session)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("wire 3 Info error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Info succeeded despite an expected refusal")
			}
			if tc.name == "unauthorized stays hidden" {
				var dsErr *ds.Error
				if !errors.As(err, &dsErr) || dsErr.Code != tc.want {
					t.Fatalf("unauthorized reader got %v, want E_NOT_FOUND", err)
				}
				return
			}
			var upgrade *capability.UpgradeRequired
			if tc.want != "E_VERSION" || !errors.As(err, &upgrade) {
				t.Fatalf("authorized wire 2 caller got %v, want typed E_VERSION refusal", err)
			}
		})
	}
}

func TestCommunityCapabilityUsesCurrentInstallationAndRefusesUploadsBeforeWrite(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	wireTwo := g.session
	wireTwo.WireVersion = 2
	if _, err := h.ds.Info(ctx, g.id, wireTwo); err != nil {
		t.Fatalf("unscoped DM-like group was gated: %v", err)
	}

	communityID, owner := id.New(), g.session.UserID
	now := h.clk.Now().Unix()
	if err := h.repo.CreateCommunity(ctx, store.CommunityRow{
		ID: communityID, Owner: owner, Name: "capability", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	installation := store.BotInstallationRow{
		ID: id.New(), CommunityID: communityID, State: store.BotInstallationReady,
		Revision: 1, ApprovedBy: owner, ApprovedAt: now, Created: now, Updated: now,
	}
	if err := h.repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatalf("CreateBotInstallation: %v", err)
	}
	// The committed MLS fixture is signed for a nil-community DM. Override only the stored binding
	// after registration to isolate the post-registration admission path; do not alter its signed
	// GroupInfo or use this setup to claim coverage of community-bound registration.
	h.repo.bindCommunity(g.id, communityID)
	for _, wireVersion := range []uint64{0, 2} {
		session := g.session
		session.WireVersion = wireVersion
		if _, err := h.ds.Info(ctx, g.id, session); err == nil {
			t.Fatalf("wire %d Info passed for an enabled community", wireVersion)
		} else {
			var upgrade *capability.UpgradeRequired
			if !errors.As(err, &upgrade) {
				t.Fatalf("wire %d Info error = %v, want typed upgrade refusal", wireVersion, err)
			}
		}
	}
	wireThree := g.session
	wireThree.WireVersion = 3
	if _, err := h.ds.Info(ctx, g.id, wireThree); err != nil {
		t.Fatalf("wire 3 Info refused for enabled community: %v", err)
	}
	if _, err := h.ds.Upload(ctx, wireTwo, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0); err == nil {
		t.Fatal("wire 2 Upload passed for an enabled community")
	} else {
		var upgrade *capability.UpgradeRequired
		if !errors.As(err, &upgrade) {
			t.Fatalf("wire 2 Upload error = %v, want typed upgrade refusal", err)
		}
	}
	messages, err := h.repo.ListAppMessages(ctx, g.id, 0, 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("refused Upload wrote app messages: rows=%d err=%v", len(messages), err)
	}

	installation.State = store.BotInstallationDisabled
	installation.Revision = 2
	installation.Updated = now + 1
	if err := h.repo.UpdateBotInstallation(ctx, installation.ID, 1, installation); err != nil {
		t.Fatalf("UpdateBotInstallation(disabled): %v", err)
	}
	if _, err := h.ds.Info(ctx, g.id, wireTwo); err != nil {
		t.Fatalf("wire 2 stayed refused after the installation was disabled: %v", err)
	}
}
