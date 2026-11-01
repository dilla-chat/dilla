package api_test

import (
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/permbits"
)

// The Bot decodes authority with permbits.All; it must be exactly PermAll (fix round 1, S-H).
func TestPermAllIsTheSharedBotMask(t *testing.T) {
	if uint64(api.PermAll) != permbits.All || uint64(api.PermControlMusic) != permbits.ControlMusic || uint64(api.PermViewChannel) != permbits.ViewChannel {
		t.Fatalf("api.PermAll = %#x, permbits.All = %#x", uint64(api.PermAll), permbits.All)
	}
}

// control_music (bit 25, Task 10c) is a channel-scoped bit: a role grants it community-wide, a
// channel overwrite can deny or grant it in one voice channel, owners and administrators always
// hold it, and a member who cannot see the channel holds nothing there.
func TestControlMusicResolvesPerChannelWithOverwrites(t *testing.T) {
	owner, user := id.New(), id.New()
	base := api.PermViewChannel | api.PermConnect | api.PermSpeak
	djRole := roleSpec{name: "dj", position: 1, allow: api.PermControlMusic, held: true}
	everyone := roleSpec{name: "@everyone", position: 0, allow: base}

	for _, tc := range []struct {
		name  string
		roles []roleSpec
		ows   []owSpec
		as    id.ID
		want  bool
	}{
		{name: "role grants it", roles: []roleSpec{everyone, djRole}, as: user, want: true},
		{name: "no role grants it", roles: []roleSpec{everyone}, as: user},
		{name: "role overwrite denies it in this channel", roles: []roleSpec{everyone, djRole},
			ows: []owSpec{{kind: 0, rolePos: 1, deny: api.PermControlMusic}}, as: user},
		{name: "@everyone overwrite denies it in this channel", roles: []roleSpec{everyone, djRole},
			ows: []owSpec{{kind: 0, rolePos: 0, deny: api.PermControlMusic}}, as: user},
		{name: "the DJ role's own overwrite restores it over an @everyone deny", roles: []roleSpec{everyone, djRole},
			ows: []owSpec{{kind: 0, rolePos: 0, deny: api.PermControlMusic}, {kind: 0, rolePos: 1, allow: api.PermControlMusic}}, as: user, want: true},
		{name: "user overwrite denies it", roles: []roleSpec{everyone, djRole},
			ows: []owSpec{{kind: 1, userID: user, deny: api.PermControlMusic}}, as: user},
		{name: "user overwrite grants it in this channel only", roles: []roleSpec{everyone},
			ows: []owSpec{{kind: 1, userID: user, allow: api.PermControlMusic}}, as: user, want: true},
		{name: "hidden channel holds nothing", roles: []roleSpec{everyone, djRole},
			ows: []owSpec{{kind: 1, userID: user, deny: api.PermViewChannel}}, as: user},
		{name: "owner always holds it", roles: []roleSpec{everyone},
			ows: []owSpec{{kind: 0, rolePos: 0, deny: api.PermControlMusic}}, as: owner, want: true},
		{name: "administrator always holds it", roles: []roleSpec{everyone, {name: "admin", position: 2, allow: api.PermAdministrator, held: true}},
			ows: []owSpec{{kind: 1, userID: user, deny: api.PermControlMusic}}, as: user, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot(t, owner, tc.as, tc.roles, tc.ows)
			if got := s.ResolveChannel(tc.as).Has(api.PermControlMusic); got != tc.want {
				t.Fatalf("control_music in channel = %v, want %v", got, tc.want)
			}
		})
	}
}

// The managed Bot's account never holds control_music, even if its role or an overwrite says so:
// DJ authority is a human member's, and the Bot cannot approve itself.
func TestBotAccountNeverHoldsControlMusic(t *testing.T) {
	owner, bot := id.New(), id.New()
	s := snapshot(t, owner, bot, []roleSpec{
		{name: "@everyone", position: 0, allow: api.PermViewChannel | api.PermConnect | api.PermSpeak},
		{name: "bot", position: 1, allow: api.PermControlMusic, held: true},
	}, []owSpec{{kind: 1, userID: bot, allow: api.PermControlMusic}})
	s.Bot = true
	if s.Resolve(bot).Has(api.PermControlMusic) || s.ResolveChannel(bot).Has(api.PermControlMusic) {
		t.Fatal("bot account resolved control_music")
	}
}

func TestControlMusicIsAKnownChannelBit(t *testing.T) {
	if api.PermControlMusic != 1<<25 {
		t.Fatalf("PermControlMusic = %#x, want bit 25 (protocol/09 § Permissions)", api.PermControlMusic)
	}
	if api.PermAll&api.PermControlMusic == 0 {
		t.Fatal("PermAll masks control_music away on read")
	}
}
