package spotify

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPCMFrameMatchesHelper48000Stereo20Milliseconds(t *testing.T) {
	const want = 48000 * 2 * 2 / 50
	if PCMBytesPerFrame != want {
		t.Fatalf("PCM frame bytes = %d, want %d for 48 kHz stereo 20 ms", PCMBytesPerFrame, want)
	}
}

func credential(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "credential")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("private-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
func TestStartReturnsOnlyAfterFirstPCMAndPlaybackStreamsRawAudio(t *testing.T) {
	helper, _ := fakeProtocolHelper(t, true)
	a, err := New(Config{HelperPath: helper, StateDir: t.TempDir(), Credential: credential(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	p, err := a.Start(ctx, "spotify:track:0123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())
	buf := make([]byte, PCMBytesPerFrame)
	if _, err := io.ReadFull(p, buf); err != nil {
		t.Fatal(err)
	}
	if len(buf) != PCMBytesPerFrame {
		t.Fatalf("PCM length = %d", len(buf))
	}
}

func TestStartFailsOnSanitizedHelperError(t *testing.T) {
	a, err := New(Config{HelperPath: mustFakeProtocolHelper(t, false), StateDir: t.TempDir(), Credential: credential(t)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Start(t.Context(), "spotify:track:0123456789012345678901")
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("Start error = %v", err)
	}
	if !errors.Is(err, ErrPlaybackFailed) || !strings.Contains(err.Error(), "authorization/provider_unavailable") {
		t.Fatalf("missing safe helper failure classification: %v", err)
	}
}

func TestStartRejectsHelperWithOutOfOrderReadinessPhases(t *testing.T) {
	helper, controlFile := fakeProtocolHelper(t, true)
	statusFile := filepath.Join(filepath.Dir(controlFile), "status")
	statusFrames := append(frameStatus(`{"ok":true,"phase":"authorization","code":null}`), frameStatus(`{"ok":true,"phase":"playback","code":null}`)...)
	if err := os.WriteFile(statusFile, statusFrames, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{HelperPath: helper, StateDir: t.TempDir(), Credential: credential(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(t.Context(), "spotify:track:0123456789012345678901"); !errors.Is(err, ErrHelperContractUnsupported) {
		t.Fatalf("Start with out-of-order phases = %v, want ErrHelperContractUnsupported", err)
	}
}

func TestStartDrainsStatusAndPCMBeforeReapingFastExit(t *testing.T) {
	helper, _ := fakeProtocolHelper(t, true, true)
	for i := 0; i < 30; i++ {
		a, err := New(Config{HelperPath: helper, StateDir: t.TempDir(), Credential: credential(t)})
		if err != nil {
			t.Fatal(err)
		}
		p, err := a.Start(t.Context(), "spotify:track:0123456789012345678901")
		if err != nil {
			t.Fatalf("iteration %d Start: %v", i, err)
		}
		buf := make([]byte, PCMBytesPerFrame)
		if _, err := io.ReadFull(p, buf); err != nil {
			t.Fatalf("iteration %d first PCM: %v", i, err)
		}
		if err := p.Close(); err != nil {
			t.Fatalf("iteration %d close: %v", i, err)
		}
	}
}

func TestConsecutiveStartsReceiveFreshCredentialStreams(t *testing.T) {
	helper, _ := fakeProtocolHelper(t, true)
	secret := credential(t)
	a, err := New(Config{HelperPath: helper, StateDir: t.TempDir(), Credential: secret})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		p, err := a.Start(t.Context(), "spotify:track:0123456789012345678901")
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		if err := p.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
}

func TestReadReportsFailureWhenHelperExitsNonzero(t *testing.T) {
	helper, _ := fakeProtocolHelper(t, true, true)
	script, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte(strings.Replace(string(script), "exit 0", "exit 7", 1)), 0700); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{HelperPath: helper, StateDir: t.TempDir(), Credential: credential(t)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Start(t.Context(), "spotify:track:0123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, PCMBytesPerFrame)
	if _, err := io.ReadFull(p, buf); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(p, buf); !errors.Is(err, ErrPlaybackFailed) {
		t.Fatalf("Read after failed child = %v, want ErrPlaybackFailed", err)
	}
	_ = p.Close()
}

func TestReadReportsEOFWhenTrackCompletesNormally(t *testing.T) {
	helper, _ := fakeProtocolHelper(t, true, true)
	a, err := New(Config{HelperPath: helper, StateDir: t.TempDir(), Credential: credential(t)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Start(t.Context(), "spotify:track:0123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, PCMBytesPerFrame)
	if _, err := io.ReadFull(p, buf); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("completed track Read = (%d, %v), want EOF", n, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPlaybackControlsAreFramedAndStopWaitsForChild(t *testing.T) {
	helper, controlFile := fakeProtocolHelper(t, true)
	a, err := New(Config{HelperPath: helper, StateDir: t.TempDir(), Credential: credential(t)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Start(t.Context(), "spotify:track:0123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for _, control := range []func(context.Context) error{p.Pause, p.Resume, func(c context.Context) error { return p.Seek(c, 1234) }} {
		if err := control(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx); err != nil && !errors.Is(err, ErrStopped) {
		t.Fatalf("second Stop = %v", err)
	}
	wire, err := os.ReadFile(controlFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{"op":"pause"}`, `{"op":"resume"}`, `{"op":"seek","position_ms":1234}`, `{"op":"stop"}`}
	for _, item := range want {
		if len(wire) < 4 {
			t.Fatalf("control stream truncated before %q", item)
		}
		n := int(binary.BigEndian.Uint32(wire[:4]))
		wire = wire[4:]
		if n != len(item) || len(wire) < n || string(wire[:n]) != item {
			t.Fatalf("control frame = %q, want %q", wire[:min(len(wire), n)], item)
		}
		wire = wire[n:]
	}
	if len(wire) != 0 {
		t.Fatalf("unexpected trailing control bytes: %x", wire)
	}
}

func TestParseStatusRejectsMalformedFrames(t *testing.T) {
	for _, input := range [][]byte{
		[]byte("bad"),
		append([]byte{0, 0, 1, 1}, []byte(`{"ok":true}`)...),
		frameStatus(`{"ok":true,"phase":"playback","code":null,"secret":"private-token"}`),
		frameStatus(`{"ok":false,"phase":"secret","code":"private-token"}`),
		frameStatus(`{"ok":true,"phase":"playback","code":null}` + strings.Repeat(" ", MaxStatusLine)),
	} {
		if _, err := ParseStatus(strings.NewReader(string(input))); err == nil {
			t.Fatalf("ParseStatus accepted malformed frame %q", input[:min(len(input), 64)])
		}
	}
}

func frameStatus(raw string) []byte {
	out := make([]byte, 4+len(raw))
	binary.BigEndian.PutUint32(out, uint32(len(raw)))
	copy(out[4:], raw)
	return out
}

func fakeProtocolHelper(t *testing.T, withPCM bool, exitAfterPCM ...bool) (string, string) {
	t.Helper()
	// A deterministic shell child emits the helper wire protocol from fixture files.
	dir := t.TempDir()
	status := append(frameStatus(`{"ok":true,"phase":"startup","code":null}`), frameStatus(`{"ok":true,"phase":"authorization","code":null}`)...)
	status = append(status, frameStatus(`{"ok":true,"phase":"playback","code":null}`)...)
	if !withPCM {
		status = frameStatus(`{"ok":false,"phase":"authorization","code":"provider_unavailable"}`)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), status, 0600); err != nil {
		t.Fatal(err)
	}
	// The Rust helper verifies this same fixture against its real writer.
	pcm, err := os.ReadFile("../../../../crates/dilla-spotify-helper/testdata/pcm-v1.frame")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pcm"), pcm, 0600); err != nil {
		t.Fatal(err)
	}
	body := `#!/bin/sh
[ "$#" -eq 1 ] && [ "$1" = "spotify:track:0123456789012345678901" ] || exit 20
[ "$(cat <&3)" = "private-token" ] || exit 21
cat "` + filepath.Join(dir, "status") + `" >&2
`
	controlFile := filepath.Join(dir, "controls")
	if withPCM {
		body += `cat "` + filepath.Join(dir, "pcm") + `"; `
		if len(exitAfterPCM) > 0 && exitAfterPCM[0] {
			body += `exit 0`
		} else {
			body += `cat >"` + controlFile + `"`
		}
	} else {
		body += `exit 1`
	}
	path := filepath.Join(dir, "helper")
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path, controlFile
}

func mustFakeProtocolHelper(t *testing.T, withPCM bool) string {
	path, _ := fakeProtocolHelper(t, withPCM)
	return path
}

func TestHelperPlaybackFailureClassifiesOnlyTrackSpecificStatus(t *testing.T) {
	for _, tc := range []struct {
		phase, code string
		permanent   bool
	}{
		{"playback", "provider_unavailable", true},
		{"authorization", "provider_connect", false},
		{"authorization", "provider_connect_timeout", false},
		{"startup", "build_configuration", false},
		{"playback", "playback_timeout", false},
		{"playback", "invalid_request", false},
		{"authorization", "provider_unavailable", false},
		{"playback", "credential_unavailable", false},
		{"playback", "internal", false},
		{"playback", "cancelled", false},
	} {
		t.Run(tc.phase+"/"+tc.code, func(t *testing.T) {
			helper := mustFakeProtocolHelper(t, false)
			status := frameStatus(`{"ok":false,"phase":"` + tc.phase + `","code":"` + tc.code + `"}`)
			if err := os.WriteFile(filepath.Join(filepath.Dir(helper), "status"), status, 0600); err != nil {
				t.Fatal(err)
			}
			adapter, err := New(Config{HelperPath: helper, StateDir: t.TempDir(), Credential: credential(t)})
			if err != nil {
				t.Fatal(err)
			}
			_, failure := adapter.Start(t.Context(), "spotify:track:0123456789012345678901")
			capability, ok := failure.(interface{ PermanentPlaybackFailure() bool })
			if (ok && capability.PermanentPlaybackFailure()) != tc.permanent {
				t.Fatalf("permanent=%v want %v: %v", ok && capability.PermanentPlaybackFailure(), tc.permanent, failure)
			}
			if !errors.Is(failure, ErrPlaybackFailed) {
				t.Fatalf("lost playback sentinel: %v", failure)
			}
			if got := FailureClass(failure); got != tc.code {
				t.Fatalf("helper status class=%q want=%q: %v", got, tc.code, failure)
			}
		})
	}
}
