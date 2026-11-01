//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/service"
)

func TestSpotifyPlaybackSetupReportsHelperAndStatePrerequisitesSeparately(t *testing.T) {
	if state, reason := spotifyPlaybackSetup(config{}); state != service.ProviderMissingRequirements || reason != service.ProviderReasonHelperMissing {
		t.Fatalf("empty configuration = %d/%d", state, reason)
	}
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "spotify-helper")
	if err := os.WriteFile(helper, []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if state, reason := spotifyPlaybackSetup(config{SpotifyHelperExecutable: helper, StateDir: filepath.Join(dir, "missing")}); state != service.ProviderMissingRequirements || reason != service.ProviderReasonStateDirectoryUnavailable {
		t.Fatalf("missing state directory = %d/%d", state, reason)
	}
	if state, reason := spotifyPlaybackSetup(config{SpotifyHelperExecutable: helper, StateDir: stateDir}); state != service.ProviderMissingRequirements || reason != service.ProviderReasonDependenciesMissing {
		t.Fatalf("missing OAuth application = %d/%d", state, reason)
	}
	if state, reason := spotifyPlaybackSetup(config{SpotifyHelperExecutable: helper, StateDir: stateDir, SpotifyClientID: "app-client", SpotifyRedirectURL: "https://dilla.test/settings/bot/music/spotify/callback"}); state != service.ProviderConfigured || reason != service.ProviderReasonNone {
		t.Fatalf("configured helper = %d/%d", state, reason)
	}
}
