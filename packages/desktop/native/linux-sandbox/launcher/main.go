//go:build linux && cgo

// This entry remains deliberately unconfigured and is not selected by AppRun.
// Final packaging must replace this compilation unit with reviewed generated
// constants and verified EmbeddedCapsule construction before it can ship.
package main

import (
	"fmt"
	"os"

	"github.com/jonasthim/dilla/packages/desktop/native/linux-sandbox/frontend"
)

func main() {
	var compiled frontend.EmbeddedCapsule
	if err := frontend.DispatchLaunch(os.Args[1:], compiled); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Dilla sandbox prerequisites are unavailable.")
		os.Exit(1)
	}
}
