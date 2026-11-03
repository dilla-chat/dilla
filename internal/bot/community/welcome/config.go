package welcome

import (
	"context"
	"errors"
	"fmt"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const configKey = "dilla.bot.welcome.config.v1"

// Config is the community-scoped encrypted-state configuration consumed by the
// runtime. A missing row means explicitly disabled; malformed or tombstoned
// state is an error so event history cannot advance past unknown settings.
type Config struct {
	ChannelID  id.ID
	Template   string
	ServerName string
}

var ErrUnconfigured = errors.New("welcome is not configured")

func LoadConfig(ctx context.Context, n Native) (Config, error) {
	if n == nil {
		return Config{}, errors.New("missing welcome state")
	}
	v, err := n.StateGet(ctx, stateArea, []byte(configKey))
	if err != nil {
		return Config{}, fmt.Errorf("read welcome configuration: %w", err)
	}
	if v.Tombstone {
		return Config{}, errors.New("welcome configuration tombstoned")
	}
	if v.Revision == 0 {
		return Config{}, ErrUnconfigured
	}
	var row []any
	if cborx.Unmarshal(v.Value, &row) != nil || len(row) != 4 || row[0] != uint64(1) {
		return Config{}, errors.New("malformed welcome configuration")
	}
	channel, ok := row[1].([]byte)
	if !ok || len(channel) != 16 {
		return Config{}, errors.New("malformed welcome channel")
	}
	template, ok := row[2].(string)
	if !ok {
		return Config{}, errors.New("malformed welcome template")
	}
	server, ok := row[3].(string)
	if !ok {
		return Config{}, errors.New("malformed welcome server name")
	}
	var out Config
	copy(out.ChannelID[:], channel)
	out.Template, out.ServerName = template, server
	if _, err = Preview(out.Template, "member", out.ServerName, "channel"); err != nil {
		return Config{}, fmt.Errorf("invalid welcome configuration: %w", err)
	}
	if out.ChannelID.IsZero() {
		return Config{}, errors.New("invalid welcome channel")
	}
	return out, nil
}

// ConfiguredEventHandler reads encrypted settings for each durable event.
// Unconfigured events are safely ignored only after StateGet proves the row is
// absent. Configured events require a processor; nil means prerequisites have
// not been wired and leaves the event pending.
func ConfiguredEventHandler(n Native, processor func(context.Context, Config) (*Processor, error)) func(context.Context, protocol.Event) error {
	return ConfiguredEventHandlerWithJoinRole(n, nil, processor)
}

// JoinRoleGranter handles automatic role policy independently of welcome text
// configuration. Implementations must verify the exact current episode.
type JoinRoleGranter interface {
	GrantJoinRole(context.Context, id.ID, id.ID) error
}

func ConfiguredEventHandlerWithJoinRole(n Native, granter JoinRoleGranter, processor func(context.Context, Config) (*Processor, error)) func(context.Context, protocol.Event) error {
	return ConfiguredEventHandlerWithCapabilities(n, granter, true, processor)
}

// ConfiguredEventHandlerWithCapabilities keeps event validation and optional
// join-role processing active while honoring a revoked Welcome capability.
func ConfiguredEventHandlerWithCapabilities(n Native, granter JoinRoleGranter, welcomeEnabled bool, processor func(context.Context, Config) (*Processor, error)) func(context.Context, protocol.Event) error {
	return func(ctx context.Context, event protocol.Event) error {
		if protocol.ValidateEventContext(event, n.Binding().Community) != nil {
			return protocol.ErrContext
		}
		if _, err := protocol.EncodeEvent(event); err != nil {
			return err
		}
		switch event.Kind {
		case protocol.EventInstallation, protocol.EventPermission, protocol.EventChannel, protocol.EventModeration, protocol.EventRolePanel, protocol.EventDeviceList:
			// The service reconciles current installation and membership state
			// before polling this durable stream. These events belong to other
			// handlers in the composed dispatcher or need no welcome action here.
			return nil
		case protocol.EventMemberJoin, protocol.EventMemberLeave:
			// Handled below.
		default:
			return fmt.Errorf("welcome handler does not own structural event kind %d", event.Kind)
		}
		if event.Kind == protocol.EventMemberJoin && granter != nil {
			member, okMember := event.Data[0].(id.ID)
			episode, okEpisode := event.Data[1].(id.ID)
			if !okMember || !okEpisode || member.IsZero() || episode.IsZero() {
				return protocol.ErrInvalid
			}
			if err := granter.GrantJoinRole(ctx, member, episode); err != nil {
				return err
			}
		}
		if !welcomeEnabled {
			return nil
		}
		cfg, err := LoadConfig(ctx, n)
		if errors.Is(err, ErrUnconfigured) {
			return nil
		}
		if err != nil {
			return err
		}
		if event.Kind == protocol.EventMemberLeave {
			return ProcessLeave(ctx, n, event)
		}
		if processor == nil {
			return errors.New("configured welcome is pending: authenticated current-membership and member-profile APIs are unavailable")
		}
		p, err := processor(ctx, cfg)
		if err != nil {
			return err
		}
		if p == nil {
			return errors.New("configured welcome processor is unavailable")
		}
		return p.Process(ctx, event)
	}
}

// ConfigStateChange is the canonical value a future authenticated settings
// writer must persist under ConfigStateKey using native state area 8.
func ConfigStateChange(cfg Config) (native.StateChange, error) {
	if cfg.ChannelID.IsZero() || len(cfg.Template) == 0 || len(cfg.ServerName) == 0 {
		return native.StateChange{}, errors.New("incomplete welcome configuration")
	}
	if _, err := Preview(cfg.Template, "member", cfg.ServerName, "channel"); err != nil {
		return native.StateChange{}, err
	}
	body, err := cborx.Marshal([]any{uint64(1), cfg.ChannelID, cfg.Template, cfg.ServerName})
	if err != nil {
		return native.StateChange{}, err
	}
	return native.StateChange{Area: stateArea, Key: []byte(configKey), Value: body}, nil
}
