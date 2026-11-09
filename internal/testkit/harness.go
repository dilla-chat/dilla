// Package testkit starts an in-process dillad and runs the Rust dilla-testkit binary against it.
//
// The Rust clients live OUTSIDE the process, and they must: CGO_ENABLED=0 forbids linking them
// in, and a client that shares the server's address space would share its clock, its RNG and its
// bugs. They speak the same wire the real client will.
package testkit

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/dillad/dilladtest"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/sfu/sfutest"
	"github.com/dilla-chat/dilla/internal/store"
)

// SeedUser is one account the harness creates before a scenario starts. It carries the per-device
// material dilladtest needs, because nothing downstream can invent it.
type SeedUser struct {
	Username  string
	Display   string
	Bot       bool
	UMKPub    string
	SSKPub    string
	SigUMKSSK string
	Devices   []dilladtest.Device
}

type Options struct {
	// Binary is the dilla-testkit executable; the default comes from $DILLA_TESTKIT.
	Binary string
	// DataDir holds the SQLite file; use t.TempDir().
	DataDir string
	// CorePath is the wasm32-wasip1 dilla-core-wasi the instance validates handshakes in; the
	// default is internal/mlswasi/testdata/dilla_core_wasi.wasm, where every other Go test finds it.
	CorePath string
	// Clock is the instance clock; nil means a fake clock at the current wall-clock second (see
	// dilladtest.HostOptions.Clock for why not a fixed date).
	Clock    *clock.Fake
	Seed     []SeedUser
	LogLevel string
	// ProductionACL runs the instance with the seams production wires (dilladtest.HostOptions):
	// the scenario builds community structure and its kicks and revocations go through /v1.
	ProductionACL bool
	// SFU starts the harness call service for native voice signal fixtures.
	SFU bool
}

type Harness struct {
	host    *dilladtest.Host
	public  *httptest.Server
	control *httptest.Server
	binary  string
}

// Start builds a dillad.Server in the test process on a random loopback port, over a temp-file
// SQLite database and a clock.Fake, and mounts the /debug control listener as a SECOND listener —
// never a route on the public mux.
func Start(tb testing.TB, o Options) *Harness {
	tb.Helper()
	binary := o.Binary
	if binary == "" {
		binary = os.Getenv("DILLA_TESTKIT")
	}
	if binary == "" {
		return unavailable(tb, "DILLA_TESTKIT is unset: build it with "+
			"`cargo build -p dilla-testkit --release` and export DILLA_TESTKIT=target/release/dilla-testkit")
	}
	if _, err := os.Stat(binary); err != nil { //nolint:gosec // G703: binary is the DILLA_TESTKIT environment variable, a test-harness input
		return unavailable(tb, fmt.Sprintf("DILLA_TESTKIT=%s is not usable: %v", binary, err))
	}
	core := o.CorePath
	if core == "" {
		core = defaultCorePath()
	}
	if _, err := os.Stat(core); err != nil {
		tb.Fatalf("%s is missing: build it with\n"+
			"  cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked\n"+
			"and copy it there (CI downloads the rust-wasi job's artifact): %v", core, err)
	}

	// Free ports, never the 7880/7882 default: parallel shards and jobs each start their own SFU.
	var sfuPort, sfuUDPPort int
	if o.SFU {
		sfuPort, sfuUDPPort = sfutest.FreePorts(tb)
	}
	host, err := dilladtest.NewHost(context.Background(), dilladtest.HostOptions{
		DataDir:       o.DataDir,
		CorePath:      core,
		Clock:         o.Clock,
		LogLevel:      o.LogLevel,
		ProductionACL: o.ProductionACL,
		SFU:           o.SFU,
		SFUPort:       sfuPort,
		SFUUDPPort:    sfuUDPPort,
	})
	if err != nil {
		tb.Fatalf("dilladtest.NewHost: %v", err)
	}
	h := &Harness{
		host:    host,
		public:  httptest.NewServer(host.Handler()),
		control: httptest.NewServer(dilladtest.ControlHandler(host)),
		binary:  binary,
	}
	if len(o.Seed) > 0 {
		if _, err := dilladtest.SeedUsers(context.Background(), host.Server(), toSeed(o.Seed)); err != nil {
			h.Stop()
			tb.Fatalf("SeedUsers: %v", err)
		}
	}
	return h
}

// RequiredEnv names the variable that, when set to anything, makes a missing or unusable dilla-testkit a failure rather
// than a skip. CI sets it for the run that builds the binary: there a skip would pass every
// accepted-commit test and every chaos scenario without running one.
const RequiredEnv = "DILLA_TESTKIT_REQUIRED"

// unavailable skips with reason — a developer without the binary still runs the rest of the
// module — unless RequiredEnv is set, and then fails with it.
func unavailable(tb testing.TB, reason string) *Harness {
	tb.Helper()
	if os.Getenv(RequiredEnv) != "" {
		tb.Fatalf("%s, and %s is set: the harness is required here, so this is a failure, not a skip",
			reason, RequiredEnv)
		return nil
	}
	tb.Skip(reason)
	return nil
}

// defaultCorePath is internal/mlswasi/testdata/dilla_core_wasi.wasm, resolved from this file
// rather than from the working directory, so a caller in any package finds the same artefact.
func defaultCorePath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("..", "mlswasi", "testdata", "dilla_core_wasi.wasm")
	}
	return filepath.Join(filepath.Dir(file), "..", "mlswasi", "testdata", "dilla_core_wasi.wasm")
}

func (h *Harness) BaseURL() string    { return h.public.URL }
func (h *Harness) ControlURL() string { return h.control.URL }

// Invite is the code every scenario client redeems (DILLA_TESTKIT_INVITE).
func (h *Harness) Invite() string { return h.host.Invite() }

// Server is the instance as it stands; restore_snapshot replaces it.
func (h *Harness) Server() *dillad.Server        { return h.host.Server() }
func (h *Harness) Repo() store.Repository        { return h.host.Server().Repo() }
func (h *Harness) DS() *ds.DS                    { return h.host.Server().DS() }
func (h *Harness) Gateway() *gateway.Gateway     { return h.host.Server().Gateway() }
func (h *Harness) Advance(d time.Duration) error { return h.host.Advance(context.Background(), d) }

// InstallVoiceSession gives a native peer a real stored call and a deterministic live SFU
// participant. The production audience resolver still checks both before op 52 succeeds.
func (h *Harness) InstallVoiceSession(tb testing.TB, callID, channelID, deviceID id.ID) {
	tb.Helper()
	room := "test-voice-" + callID.String()
	if err := h.Repo().PutVoiceSession(context.Background(), store.VoiceSessionRow{
		CallID: callID, ChannelID: channelID, LivekitRoom: room, Started: time.Now().Unix(),
	}); err != nil {
		tb.Fatalf("PutVoiceSession: %v", err)
	}
	h.host.StubVoiceParticipant(room, deviceID.String())
}

func (h *Harness) AddVoiceParticipant(callID, deviceID id.ID) {
	h.host.StubVoiceParticipant("test-voice-"+callID.String(), deviceID.String())
}

// DebugState is what GET /debug/state reports, read in-process.
func (h *Harness) DebugState(tb testing.TB) dilladtest.DebugState {
	tb.Helper()
	state, err := dilladtest.State(context.Background(), h.host.Server())
	if err != nil {
		tb.Fatalf("DebugState: %v", err)
	}
	return state
}

func (h *Harness) Stop() {
	h.control.Close()
	// CloseClientConnections first: a gateway socket is hijacked, so Close would wait on the
	// server's shutdown to end it, and the server is shut down after the listener.
	h.public.CloseClientConnections()
	h.public.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = h.host.Close(ctx)
}

// toSeed converts the harness's Options.Seed into the control listener's wire shape.
func toSeed(users []SeedUser) []dilladtest.SeedRequest {
	out := make([]dilladtest.SeedRequest, 0, len(users))
	for _, u := range users {
		out = append(out, dilladtest.SeedRequest{
			Username: u.Username, Display: u.Display, Bot: u.Bot,
			UMKPub: u.UMKPub, SSKPub: u.SSKPub, SigUMKSSK: u.SigUMKSSK, Devices: u.Devices,
		})
	}
	return out
}
