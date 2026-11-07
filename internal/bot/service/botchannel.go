package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 17: the Bot posts only into Bot channels. The community's marks come from the Bot's event feed
// (Task 16); a community registers them once (SetPostGate) and every send, whichever feature or webhook makes it,
// is refused at the send layer for a group whose channel is not marked. Features also ask first (CheckBotChannel),
// so a feature aimed at an unmarked channel posts nothing and says why: needs_bot_channel.

// NeedsBotChannelCode is the reason a feature reports when its channel is not a Bot channel.
const NeedsBotChannelCode = "needs_bot_channel"

// ErrNeedsBotChannel matches (errors.Is) every refusal because the target is not a marked Bot channel.
var ErrNeedsBotChannel = errors.New("needs_bot_channel")

// ErrBotChannelsUnknown is a temporary refusal: the feed has not applied its first snapshot, so the marks are not
// known yet. A caller retries; nothing is posted meanwhile.
var ErrBotChannelsUnknown = errors.New("bot channels not known yet")

// NeedsBotChannelError names the channel (when known) a post was refused for.
type NeedsBotChannelError struct{ Channel id.ID }

func (e *NeedsBotChannelError) Error() string        { return NeedsBotChannelCode }
func (e *NeedsBotChannelError) Is(target error) bool { return target == ErrNeedsBotChannel }
func (e *NeedsBotChannelError) FailureClass() string { return NeedsBotChannelCode }

// BotChannelMarks is the community's Bot channels as the feed last applied them, and whether a snapshot has been
// applied. BotChannels (membership) and BotChannelsFunc satisfy it.
type BotChannelMarks interface {
	Marked() (marked map[id.ID]bool, known bool)
}

var postGates sync.Map // community id.ID -> BotChannelMarks

// SetPostGate registers the community's Bot channel marks: from then on every send into an ordinary group of that
// community must target a marked Bot channel. A nil gate removes it (v1 behaviour, tests).
func SetPostGate(community id.ID, marks BotChannelMarks) {
	if community.IsZero() {
		return
	}
	if marks == nil {
		postGates.Delete(community)
		return
	}
	postGates.Store(community, marks)
}

// CheckBotChannel answers nil when the Bot may post into channel of community: no gate registered, or the channel is a
// marked Bot channel. An unmarked channel is *NeedsBotChannelError; marks not yet known is ErrBotChannelsUnknown.
func CheckBotChannel(community, channel id.ID) error {
	value, ok := postGates.Load(community)
	if !ok {
		return nil
	}
	marks, _ := value.(BotChannelMarks)
	if marks == nil {
		return ErrBotChannelsUnknown
	}
	marked, known := marks.Marked()
	if !known {
		return ErrBotChannelsUnknown
	}
	if !marked[channel] {
		return &NeedsBotChannelError{Channel: channel}
	}
	return nil
}

// NeedsBotChannelFlag is one feature the Bot could not post for because its channel is not a Bot channel.
type NeedsBotChannelFlag struct {
	Feature string
	Channel id.ID
}

var (
	flagMu sync.Mutex
	flags  = map[id.ID]map[string]id.ID{}
)

// FlagNeedsBotChannel records that feature of community could not post to channel (the admin page shows it).
func FlagNeedsBotChannel(community id.ID, feature string, channel id.ID) {
	flagMu.Lock()
	defer flagMu.Unlock()
	if flags[community] == nil {
		flags[community] = map[string]id.ID{}
	}
	flags[community][feature] = channel
}

// ClearNeedsBotChannel drops the feature's flag once it posts again.
func ClearNeedsBotChannel(community id.ID, feature string) {
	flagMu.Lock()
	defer flagMu.Unlock()
	delete(flags[community], feature)
	if len(flags[community]) == 0 {
		delete(flags, community)
	}
}

// NeedsBotChannelFlags lists the community's flagged features, by feature name.
func NeedsBotChannelFlags(community id.ID) []NeedsBotChannelFlag {
	flagMu.Lock()
	defer flagMu.Unlock()
	out := make([]NeedsBotChannelFlag, 0, len(flags[community]))
	for feature, channel := range flags[community] {
		out = append(out, NeedsBotChannelFlag{Feature: feature, Channel: channel})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Feature < out[j].Feature })
	return out
}

// HintSender sends state hints (Bot v2 Task 17): a user hint when a reminder is due, a channel hint when a poll in a
// Bot channel changes, a call hint when the music queue or the playing track changes.
type HintSender = client.HintSender

// hint sends one state hint, best effort: a hint carries no content and a missed one is recovered when the client
// compares revisions at connect, so a failure never fails the command that caused it.
func hint(ctx context.Context, sender HintSender, kind uint8, scope id.ID) {
	if sender == nil || scope.IsZero() {
		return
	}
	hintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hintTimeout)
	defer cancel()
	_ = sender.Hint(hintCtx, kind, scope)
}

const hintTimeout = 2 * time.Second

// needsBotChannelResult refuses a posting command because its channel is not a Bot channel, with the reason for the
// member, and flags the feature for the admin page.
func needsBotChannelResult(community id.ID, feature string, channel id.ID, reason string) InteractionResult {
	FlagNeedsBotChannel(community, feature, channel)
	return InteractionResult{Status: interactionStatusInvalid, Note: reason}
}

// OnlyBotChannel is the community's Bot channel when it has exactly one: the default for a feature with no configured
// channel of its own.
func OnlyBotChannel(community id.ID) (id.ID, bool) {
	value, ok := postGates.Load(community)
	if !ok {
		return id.ID{}, false
	}
	marks, _ := value.(BotChannelMarks)
	if marks == nil {
		return id.ID{}, false
	}
	marked, known := marks.Marked()
	if !known || len(marked) != 1 {
		return id.ID{}, false
	}
	for channel, on := range marked {
		if on {
			return channel, true
		}
	}
	return id.ID{}, false
}
