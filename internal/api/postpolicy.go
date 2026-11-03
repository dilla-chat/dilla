package api

import (
	"context"
	"errors"

	botaccess "github.com/dilla-chat/dilla/internal/bot/access"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// PostPolicy is the delivery service's ds.PostPolicy (web3.7 SB1, protocol/02 § Upload class): the
// host's two rules for an end-to-end encrypted upload into a community text group. Active member timeouts block every community text upload,
// including class-1 folds, before the class-1 shortcut. A class-0 upload (a post) also needs
// send_messages in the channel and waits out slow mode unless the user holds bypass_slowmode.
// Direct messages and call groups are unchecked.
type PostPolicy struct {
	Repo     store.Repository
	Resolver *Resolver
}

// Check decides one upload of class by userID into g at now (unix seconds). It remains available
// to callers that are not accepting a message. DS upload paths call CheckTx while holding the
// community and group row fences.
//
// Every upload names its class: the route refuses the two-element legacy body web3.7 accepted
// (web3.8 S1). A group whose channel is gone is refused for every class (fail closed). The wait is
// read from the user's durable mark, which Upload raises on every accepted class-0 post, so a
// pruned post still holds it.
func (p PostPolicy) Check(ctx context.Context, g store.GroupRow, userID id.ID, class uint8, now int64) error {
	return p.check(ctx, p.Repo, p.Resolver, g, userID, class, now)
}

// CheckTx runs the upload policy against the caller's locked transaction. The
// channel permissions, bot eligibility and slowmode mark therefore describe the
// same serialized state that the accepted message updates.
func (p PostPolicy) CheckTx(ctx context.Context, tx store.Repository, g store.GroupRow, userID id.ID, class uint8, now int64) error {
	return p.check(ctx, tx, NewResolver(tx), g, userID, class, now)
}

func (p PostPolicy) check(ctx context.Context, repo store.Repository, resolver *Resolver, g store.GroupRow, userID id.ID, class uint8, now int64) error {
	if g.CommunityID == nil {
		bot, err := botaccess.IsBotUser(ctx, repo, userID)
		if err != nil {
			return err
		}
		if bot {
			return server.Errorf(server.CodeForbidden, "managed bots cannot post outside their community")
		}
	} else {
		binding, bot, err := botaccess.ForUserCommunity(ctx, repo, userID, *g.CommunityID)
		if err != nil {
			if errors.Is(err, botaccess.ErrIneligible) {
				return server.Errorf(server.CodeForbidden, "managed bot installation is not eligible for this community")
			}
			return err
		}
		if bot && (binding.State != store.BotInstallationReady || binding.CommunityID != *g.CommunityID) {
			return server.Errorf(server.CodeForbidden, "managed bot installation is not ready for sends")
		}
	}
	if g.Kind != GroupText || g.CommunityID == nil {
		return nil
	}
	ch, err := repo.GetChannel(ctx, g.TargetID)
	if errors.Is(err, store.ErrNotFound) {
		// Fail closed (SB1 fix round 1): a deleted channel's group that is still open takes no
		// upload of any class. The delivery service also refuses a closed group outright.
		return server.Errorf(server.CodeForbidden, "missing permission")
	}
	if err != nil {
		return err
	}
	if resolver == nil {
		resolver = NewResolver(repo)
	}
	if _, err := repo.GetMemberTimeout(ctx, *g.CommunityID, userID, now); err == nil {
		return server.Errorf(server.CodeForbidden, "member is timed out")
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	bits, err := resolver.Resolve(ctx, userID, ch)
	if err != nil {
		return err
	}
	if class == 1 {
		return nil
	}
	if !bits.Has(PermSendMessages) {
		return server.Errorf(server.CodeForbidden, "missing permission")
	}
	if ch.SlowmodeSeconds == 0 || bits.Has(PermBypassSlowmode) {
		return nil
	}
	last, err := repo.LastClassZeroUploadAt(ctx, ch.ID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return slowmodeWait(ch.SlowmodeSeconds, last, now)
}
