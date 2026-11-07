package botsupervisor

import (
	"os"
	"testing"
)

// TestMain lets the test binary play the processes a launch involves (keeper_linux_test.go): the keeper, `bot-launch`
// and the Bot itself. Anything else runs the tests.
func TestMain(m *testing.M) {
	runTestChild()
	os.Exit(m.Run())
}
