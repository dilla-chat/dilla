package spotify

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPCMStarterLoadsCredentialWithoutPuttingItInArguments(t *testing.T) {
	helper := fakeTokenProtocolHelper(t)
	argsFile := filepath.Join(t.TempDir(), "args")
	body, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	// Record the argument vector while retaining the protocol checks in the fake.
	body = append([]byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >'"+argsFile+"'\n"), body[len("#!/bin/sh\n"):]...)
	if err := os.WriteFile(helper, body, 0700); err != nil {
		t.Fatal(err)
	}
	starter, err := NewPCMStarter(helper, t.TempDir(), func(context.Context) (string, error) {
		return "private-token", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	pcm, err := starter.Start(ctx, "spotify:track:0123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	defer pcm.Close()
	frame := make([]byte, PCMBytesPerFrame)
	if _, err := io.ReadFull(pcm, frame); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "spotify:track:0123456789012345678901\n" {
		t.Fatalf("helper arguments = %q", args)
	}
}

func TestPCMStarterSanitizesCredentialLoadErrors(t *testing.T) {
	starter, err := NewPCMStarter("/bin/true", t.TempDir(), func(context.Context) (string, error) {
		return "", os.ErrPermission
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := starter.Start(t.Context(), "spotify:track:0123456789012345678901"); err == nil || err.Error() == os.ErrPermission.Error() {
		t.Fatalf("credential error was not sanitized: %v", err)
	}
}

func TestPCMStarterCancellationReapsChildAndAllowsRestart(t *testing.T) {
	helper := fakeTokenProtocolHelper(t)
	loads := 0
	starter, err := NewPCMStarter(helper, t.TempDir(), func(context.Context) (string, error) {
		loads++
		return "private-token", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	pcm, err := starter.Start(ctx, "spotify:track:0123456789012345678901")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	frame := make([]byte, PCMBytesPerFrame)
	if _, err := io.ReadFull(pcm, frame); err != nil {
		cancel()
		_ = pcm.Close()
		t.Fatal(err)
	}
	cancel()
	_, _ = pcm.Read(frame)
	if err := pcm.Close(); err != nil {
		t.Fatalf("close cancelled child: %v", err)
	}
	next, err := starter.Start(t.Context(), "spotify:track:0123456789012345678901")
	if err != nil {
		t.Fatalf("restart after cancellation: %v", err)
	}
	if loads != 2 {
		t.Fatalf("credential loads = %d, want one fresh load per playback", loads)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

func fakeTokenProtocolHelper(t *testing.T) string {
	t.Helper()
	helper := mustFakeProtocolHelper(t, true)
	body, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.ReplaceAll(string(body), `"private-token"`, `"{\"access_token\":\"private-token\"}"`))
	if err := os.WriteFile(helper, body, 0700); err != nil {
		t.Fatal(err)
	}
	return helper
}

func TestPCMStarterPreservesCredentialLoadCause(t *testing.T) {
	starter, err := NewPCMStarter("/bin/true", t.TempDir(), func(context.Context) (string, error) { return "", os.ErrPermission })
	if err != nil {
		t.Fatal(err)
	}
	for _, start := range []bool{false, true} {
		var loadErr error
		if start {
			_, loadErr = starter.Start(t.Context(), "spotify:track:0123456789012345678901")
		} else {
			loadErr = starter.Check(t.Context())
		}
		if !errors.Is(loadErr, os.ErrPermission) || !errors.Is(loadErr, ErrCredentialUnavailable) {
			t.Fatalf("load cause lost: %v", loadErr)
		}
		if strings.Contains(loadErr.Error(), os.ErrPermission.Error()) {
			t.Fatal("raw load cause exposed")
		}
	}
}

func TestPCMStarterMissingCredentialIsPermanentButLoadFailurePauses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		token     string
		loadErr   error
		permanent bool
	}{
		{name: "unlinked", permanent: true},
		{name: "transient load", loadErr: os.ErrPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starter, err := NewPCMStarter("/bin/true", t.TempDir(), func(context.Context) (string, error) { return tc.token, tc.loadErr })
			if err != nil {
				t.Fatal(err)
			}
			for _, start := range []bool{false, true} {
				var failure error
				if start {
					_, failure = starter.Start(t.Context(), "spotify:track:0123456789012345678901")
				} else {
					failure = starter.Check(t.Context())
				}
				capability, ok := failure.(interface{ PermanentPlaybackFailure() bool })
				if (ok && capability.PermanentPlaybackFailure()) != tc.permanent {
					t.Fatalf("start=%v permanent=%v want %v: %v", start, ok && capability.PermanentPlaybackFailure(), tc.permanent, failure)
				}
			}
		})
	}
}

func TestPCMStarterPreservesMissingProviderSentinel(t *testing.T) {
	starter, err := NewPCMStarter("/bin/true", t.TempDir(), func(context.Context) (string, error) { return "", ErrProviderUnavailable })
	if err != nil {
		t.Fatal(err)
	}
	for _, start := range []bool{false, true} {
		var failure error
		if start {
			_, failure = starter.Start(t.Context(), "spotify:track:0123456789012345678901")
		} else {
			failure = starter.Check(t.Context())
		}
		if !errors.Is(failure, ErrProviderUnavailable) || !errors.Is(failure, ErrCredentialUnavailable) || !PermanentPlaybackFailure(failure) {
			t.Fatalf("missing-provider load classification lost: %v", failure)
		}
	}
}
