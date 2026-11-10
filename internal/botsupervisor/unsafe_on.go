//go:build dilla_unsafe_test

package botsupervisor

// unsafeTestBuild is true only in a binary built with `-tags dilla_unsafe_test`, which only
// TestServingDilladHoldsNoSetuidCapability does: it runs serve as root in a user namespace, where the host's root owns
// no file. It relaxes, for that binary only:
//   - the Bot files' leaf ownership: the executable, helper and wasi core need not be root's;
//   - the parent rule: no directory or symlink on their paths need be root's or unwritable by others;
//   - the root refusal: serve and the keeper run with euid 0 (cmd/dillad refuseRootKeeper).
//
// The tag is recorded in the binary's build info even with -trimpath: the release jobs fail a dillad that carries it
// (`go version -m`), and cmd/dillad's TestUnsafeTestTagStaysInItsTests keeps it out of every other tracked file.
const unsafeTestBuild = true
