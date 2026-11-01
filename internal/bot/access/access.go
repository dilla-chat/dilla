// Package access contains repository-backed eligibility checks for managed bot sessions.
// It deliberately holds no authority cache: every call reads the account, device,
// installation and runtime that currently control a bot.
package access

import (
	"context"
	"errors"
	"time"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

var ErrIneligible = errors.New("bot access: installation is not eligible")

const accountKindBot uint8 = 1

// MusicCapability is the installation capability bit used by managed-bot route authorization.
const MusicCapability uint8 = 1 << 0

// Binding describes the managed installation that owns a bot account/device.
// Capabilities are the human-approved installation mask, not permission bits.
type Binding struct {
	InstallationID id.ID
	CommunityID    id.ID
	State          uint8
	Capabilities   uint16
}

func liveState(state uint8) bool {
	return state == store.BotInstallationEnrolling || state == store.BotInstallationJoining || state == store.BotInstallationReady
}

// IsBotUser identifies the account kind for seams that receive no device id.
// It does not grant authority; callers must still require ForUserCommunity.
func IsBotUser(ctx context.Context, repo store.Repository, userID id.ID) (bool, error) {
	user, err := repo.GetUser(ctx, userID)
	if err != nil {
		return false, err
	}
	return user.Kind == accountKindBot, nil
}

// ForUserDevice returns the live installation bound to an account/device pair.
// A normal human account returns (zero, false, nil); a bot with any broken or
// incomplete binding returns ErrIneligible.
func ForUserDevice(ctx context.Context, repo store.Repository, userID, deviceID id.ID) (Binding, bool, error) {
	user, err := repo.GetUser(ctx, userID)
	if err != nil {
		return Binding{}, false, err
	}
	if user.Kind != accountKindBot {
		return Binding{}, false, nil
	}
	if user.Flags != 0 || user.DisabledAt != nil || user.DeletedAt != nil || deviceID.IsZero() {
		return Binding{}, true, ErrIneligible
	}
	device, err := repo.GetDevice(ctx, deviceID)
	if err != nil {
		return Binding{}, true, err
	}
	if device.UserID != userID || device.Tier != 0 || device.SignerTier != 0 ||
		device.RevokedAt != nil || device.QuarantinedAt != nil || len(device.DSKPub) != 32 || len(device.CredentialBlob) == 0 {
		return Binding{}, true, ErrIneligible
	}

	after := id.ID{}
	for {
		rows, err := repo.ListBotInstallationsByDevice(ctx, deviceID, after, 100)
		if err != nil {
			return Binding{}, true, err
		}
		if len(rows) == 0 {
			return Binding{}, true, ErrIneligible
		}
		for _, row := range rows {
			if row.UserID == nil || *row.UserID != userID || row.DeviceID == nil || *row.DeviceID != deviceID ||
				row.RemovedAt != nil || !liveState(row.State) || row.RuntimeID == nil {
				continue
			}
			if row.State >= store.BotInstallationJoining && row.CredentialVerifiedAt == nil {
				continue
			}
			runtime, err := repo.GetBotRuntime(ctx, *row.RuntimeID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				return Binding{}, true, err
			}
			if runtime.RevokedAt != nil || len(runtime.TokenHash) != 32 {
				continue
			}
			if err := communityMember(ctx, repo, row.CommunityID, userID); err != nil {
				return Binding{}, true, err
			}
			return Binding{InstallationID: row.ID, CommunityID: row.CommunityID,
				State: row.State, Capabilities: row.Capabilities}, true, nil
		}
		after = rows[len(rows)-1].ID
		if len(rows) < 100 {
			return Binding{}, true, ErrIneligible
		}
	}
}

// ForUserCommunity answers whether user is an eligible managed bot in community.
// It performs an indexed community lookup and verifies the device/runtime rows;
// human callers are left to the ordinary ACL path.
func ForUserCommunity(ctx context.Context, repo store.Repository, userID, communityID id.ID) (Binding, bool, error) {
	user, err := repo.GetUser(ctx, userID)
	if err != nil {
		return Binding{}, false, err
	}
	if user.Kind != accountKindBot {
		return Binding{}, false, nil
	}
	if user.Flags != 0 || user.DisabledAt != nil || user.DeletedAt != nil {
		return Binding{}, true, ErrIneligible
	}
	row, err := repo.GetActiveBotInstallationByCommunity(ctx, communityID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Binding{}, true, ErrIneligible
		}
		return Binding{}, true, err
	}
	if row.UserID == nil || *row.UserID != userID || row.DeviceID == nil {
		return Binding{}, true, ErrIneligible
	}
	if err := communityMember(ctx, repo, communityID, userID); err != nil {
		return Binding{}, true, err
	}
	binding, bot, err := ForUserDevice(ctx, repo, userID, *row.DeviceID)
	if err != nil {
		return Binding{}, true, err
	}
	if !bot || binding.InstallationID != row.ID || binding.CommunityID != communityID {
		return Binding{}, true, ErrIneligible
	}
	return binding, true, nil
}

func communityMember(ctx context.Context, repo store.Repository, communityID, userID id.ID) error {
	community, err := repo.GetCommunity(ctx, communityID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrIneligible
		}
		return err
	}
	if community.DeletedAt != nil {
		return ErrIneligible
	}
	if _, err := repo.GetMember(ctx, communityID, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrIneligible
		}
		return err
	}
	ban, err := repo.GetBan(ctx, communityID, userID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && (ban.Expires == nil || *ban.Expires > time.Now().Unix()) {
		return ErrIneligible
	}
	return nil
}

// EligibleCommunityMember checks live membership for a user without granting
// installation authority. It is used when a bound bot reads another member's
// signed device list or key package.
func EligibleCommunityMember(ctx context.Context, repo store.Repository, communityID, userID id.ID) error {
	user, err := repo.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if user.DisabledAt != nil || user.DeletedAt != nil {
		return ErrIneligible
	}
	return communityMember(ctx, repo, communityID, userID)
}
