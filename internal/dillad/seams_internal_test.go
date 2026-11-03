package dillad

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Fix wave I18: with neither seam in Options — which is what `dillad serve` passes — the delivery
// service gets the production channel source and ACL over the server's own repository, and a seam
// a harness injects is kept as given. The behaviour those two produce through the composition
// root is TestTheCompositionRootWiresTheResolverACL (seams_test.go) and the production-ACL
// testkit scenarios; this pins the one line of New a harness's doubles would otherwise hide.
func TestNewDefaultsToTheProductionDeliverySeams(t *testing.T) {
	var repo store.Repository // any value; only its identity is compared
	channels, acl := deliverySeams(Options{Repo: repo})
	if got, ok := channels.(api.StructureChannels); !ok || got.Repo != repo {
		t.Errorf("the default channel source is %T %+v, want api.StructureChannels over the repository", channels, channels)
	}
	if got, ok := acl.(api.ResolverACL); !ok || got.Repo != repo {
		t.Errorf("the default ACL is %T %+v, want api.ResolverACL over the repository", acl, acl)
	}

	injectedACL, injectedChannels := admitAll{}, admitAllChannels{}
	channels, acl = deliverySeams(Options{Repo: repo, ACL: injectedACL, Channels: injectedChannels})
	if channels != ds.Channels(injectedChannels) || acl != ds.ACL(injectedACL) {
		t.Errorf("injected seams were replaced: %T, %T", channels, acl)
	}
}

type admitAll struct{}

func (admitAll) Eligible(context.Context, id.ID, id.ID) (bool, error) { return true, nil }

type admitAllChannels struct{}

func (admitAllChannels) Channel(context.Context, id.ID) (uint8, uint8, error) {
	return 0, 0, ds.ErrNoChannel
}
func (admitAllChannels) MayRegister(context.Context, id.ID, ds.Binding) error { return nil }
