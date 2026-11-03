package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/dillad"
)

// TestMain puts the wasi core where `dillad serve` looks for it: dillad.CoreFileName beside the
// running binary, which under `go test` is this test binary (task 27a: the composition root
// compiles the core, and dilla.toml has no [mls] table to point elsewhere). The artefact is the one
// the mlswasi tests use — CI downloads the rust-wasi job's build into internal/mlswasi/testdata,
// and locally it is built with `cargo build -p dilla-core-wasi --target wasm32-wasip1 --release
// --locked` and copied there. When it is absent nothing is copied, and the serve tests fail naming
// the path New looked in.
func TestMain(m *testing.M) {
	placed, err := placeCore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cmd/dillad tests: place the wasi core:", err)
	}
	code := m.Run()
	if placed != "" {
		_ = os.Remove(placed)
	}
	os.Exit(code)
}

func placeCore() (string, error) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "mlswasi", "testdata", dillad.CoreFileName))
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(filepath.Dir(exe), dillad.CoreFileName)
	if _, err := os.Stat(dst); err == nil {
		return "", nil // someone else's; leave it
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}
