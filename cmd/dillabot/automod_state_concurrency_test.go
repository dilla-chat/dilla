//go:build linux

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

type blockingModerationSource struct {
	binding native.Binding
	read    func()
}

func (s *blockingModerationSource) Binding() native.Binding { return s.binding }
func (s *blockingModerationSource) ReadGroupEvents(context.Context, uint64, uint64) (native.GroupEventPage, error) {
	if s.read != nil {
		s.read()
	}
	return native.GroupEventPage{HighWater: 1}, nil
}

func TestAutoModerationRetirementDoesNotWaitForSettingsPersistence(t *testing.T) {
	state := newAutoModerationEventState()
	installation, community := id.New(), id.New()
	source := &blockingModerationSource{binding: native.Binding{Installation: installation, Community: community}}
	cfg := logOnlyModerationConfig()
	initial, _, _, err := state.Configure(t.Context(), source, installation, community, 0, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := state.ConfigRuntime(source, installation, community, func(moderation.Config) (service.ModerationActorAuthority, service.ModerationActionSink, error) {
		return nil, nil, nil
	})
	entered, proceed := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- config.PersistConfig(t.Context(), cfg, 0, func() error { close(entered); <-proceed; return nil })
	}()
	<-entered
	available := make(chan struct{})
	go func() {
		// The refresh must preserve the active runtime while its settings write
		// is in flight, without waiting for the persistence callback.
		current, _, _, refreshErr := state.Configure(t.Context(), source, installation, community, 0, cfg, nil, nil)
		if refreshErr != nil || current != initial {
			t.Errorf("refresh runtime=%p err=%v", current, refreshErr)
		}
		otherInstallation, otherCommunity := id.New(), id.New()
		other := &blockingModerationSource{binding: native.Binding{Installation: otherInstallation, Community: otherCommunity}}
		if _, _, _, otherErr := state.Configure(t.Context(), other, otherInstallation, otherCommunity, 0, cfg, nil, nil); otherErr != nil {
			t.Error(otherErr)
		}
		state.Disable(installation)
		close(available)
	}()
	select {
	case <-available:
	case <-time.After(time.Second):
		t.Error("settings persistence blocked registry reads or retirement")
	}
	close(proceed)
	if err := <-done; err == nil {
		t.Error("retired activation unexpectedly published state")
	}
	<-available
	state.mu.Lock()
	_, exists := state.entries[installation]
	state.mu.Unlock()
	if exists {
		t.Fatal("settings write resurrected a retired runtime")
	}
}

func TestAutoModerationInitialConfigureCannotResurrectRemovedInstallation(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(map[bool]string{false: "disable", true: "retain-only"}[retain], func(t *testing.T) {
			state := newAutoModerationEventState()
			installation, community := id.New(), id.New()
			entered, proceed := make(chan struct{}), make(chan struct{})
			var once sync.Once
			source := &blockingModerationSource{binding: native.Binding{Installation: installation, Community: community},
				read: func() { once.Do(func() { close(entered) }); <-proceed }}
			done := make(chan error, 1)
			go func() {
				_, _, _, err := state.Configure(t.Context(), source, installation, community, 0, logOnlyModerationConfig(), nil, nil)
				done <- err
			}()
			<-entered
			removed := make(chan struct{})
			go func() {
				if retain {
					state.RetainOnly(nil)
				} else {
					state.Disable(installation)
				}
				close(removed)
			}()
			select {
			case <-removed:
			case <-time.After(time.Second):
				t.Error("journal read blocked retirement")
			}
			close(proceed)
			if err := <-done; err == nil {
				t.Error("initial activation survived retirement")
			}
			<-removed
			state.mu.Lock()
			_, exists := state.entries[installation]
			state.mu.Unlock()
			if exists {
				t.Fatal("journal read resurrected a retired runtime")
			}
		})
	}
}
