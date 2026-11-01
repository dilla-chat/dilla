//go:build !windows

package main

import "os"

// The portable files exist for verification tests; this adapter launches Windows only.
func main() { os.Exit(1) }
