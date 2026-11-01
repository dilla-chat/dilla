// Package permbits holds the permission bit assignments that both dillad
// (internal/api) and the managed Bot (internal/bot) depend on. It has no
// imports so either side may use it; protocol/vectors/permissions.json is the
// wire reference and permbits_test.go checks All against it.
package permbits

const (
	// ViewChannel is api.PermViewChannel.
	ViewChannel uint64 = 1 << 0
	// ControlMusic is api.PermControlMusic.
	ControlMusic uint64 = 1 << 25
	// All is every defined permission bit (api.PermAll). A decoder that sees a
	// bit outside All is talking to a newer or broken server.
	All uint64 = 1<<26 - 1
)
