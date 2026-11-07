//go:build !dilla_testhost

package youtube

import "os"

// fixtureBackend refuses the fixture variable: a shipped Bot never serves
// fixture files, so an inherited DILLA_BOT_MUSIC_FIXTURES fails startup loudly.
func fixtureBackend(SandboxConfig) (*Backend, bool, error) {
	if os.Getenv(FixturesEnv) != "" {
		return nil, true, ErrFixturesRefused
	}
	return nil, false, nil
}
