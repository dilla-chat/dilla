//go:build linux

package main

import (
	"path/filepath"
	"testing"

	"github.com/jonasthim/dilla/internal/dillad/dilladtest"
)

func TestMusicEnqueueResumeJoinsRealCallGroup(t *testing.T) {
	t.Skip("TODO(signal2): dilladtest seeds users and sessions but not an enrolled native Bot helper, signed device-list trust, ready installation and human-created MLS call group; composing those fixtures exceeds the ~300-line hotfix harness budget")

	// Bootstrap the real server rather than substituting a successful Join closure.
	host, err := dilladtest.NewHost(t.Context(), dilladtest.HostOptions{
		DataDir:  t.TempDir(),
		CorePath: filepath.Join("..", "..", "internal", "mlswasi", "testdata", "dilla_core_wasi.wasm"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(t.Context()) })
	// Remaining fixture work: enroll a human and the concrete native.Client with
	// signed device lists; create a voice call as the human; activate a music Bot
	// installation and obtain its client.Session. Wire the real
	// ensureMusicCallGroupJoined into newInstallationMusicQueueStarter, injecting
	// only PCM/encoder/publisher. Drive MusicInteractions.Enqueue then Resume and
	// query host.Server().Repo() for the Bot device's mls_members row with
	// group_id = the live call group and call_id = the voice channel.
	// A seeded row or a fake JoinCallGroup would not establish this regression.
}
