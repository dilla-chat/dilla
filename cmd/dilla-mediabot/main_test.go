package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/sframe"
	"github.com/dilla-chat/dilla/internal/sfu"
	"github.com/dilla-chat/dilla/internal/sfu/sfutest"
)

const (
	keyHex = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"
	devA   = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	devB   = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
)

// keyFile writes the base key the way a caller must: a file only its owner can read.
func keyFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "base-key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func secretFile(t *testing.T, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTokenFileRequiresPrivateRegularCurrentOwnerBoundedFile(t *testing.T) {
	base := []string{"-url", "ws://x", "-base-key-file", "-", "-leaf", "0", "-epoch", "1", "-roster", "0:" + devA}
	good := secretFile(t, "token", "  jwt-secret\n", 0o600)
	args := append([]string{"-token-file", good}, base...)
	c, err := parseFlags(args, strings.NewReader(keyHex), &bytes.Buffer{})
	if err != nil || c.Token != "jwt-secret" {
		t.Fatalf("private token file = %q, %v", c.Token, err)
	}

	groupReadable := secretFile(t, "group-token", "jwt-secret", 0o640)
	large := secretFile(t, "large-token", strings.Repeat("x", (1<<20)+1), 0o600)
	link := filepath.Join(t.TempDir(), "token-link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "token-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot construct a FIFO: %v", err)
	}
	for _, tc := range []struct {
		name string
		path string
		want string
	}{{"group readable", groupReadable, "owner-only 0600 regular file"}, {"symlink", link, "unavailable or unsafe"}, {"fifo", fifo, "owner-only 0600 regular file"}, {"oversized", large, "too large"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(append([]string{"-token-file", tc.path}, base...), strings.NewReader(keyHex), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseFlags error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), tc.path) || strings.Contains(err.Error(), "jwt-secret") {
				t.Fatalf("token-file error disclosed path or token: %v", err)
			}
		})
	}

	t.Run("other owner", func(t *testing.T) {
		other := secretFile(t, "other-owner", "jwt-secret", 0o600)
		if err := os.Chown(other, os.Geteuid()+1, -1); err != nil {
			t.Skipf("cannot construct a file owned by another uid: %v", err)
		}
		_, err := parseFlags(append([]string{"-token-file", other}, base...), strings.NewReader(keyHex), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "owner-only 0600 regular file") {
			t.Fatalf("other-owner token file = %v", err)
		}
	})
}

func TestTokenFileFlagsAreRequiredAndMutuallyExclusive(t *testing.T) {
	key := keyFile(t, keyHex, 0o600)
	base := []string{"-url", "ws://x", "-base-key-file", key, "-leaf", "0", "-epoch", "1", "-roster", "0:" + devA}
	file := secretFile(t, "token", "jwt-secret", 0o600)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{{"neither", nil, "-token or -token-file is required"}, {"both", []string{"-token", "jwt-secret", "-token-file", file}, "mutually exclusive"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(append(append([]string{}, tc.args...), base...), strings.NewReader(""), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseFlags error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFlagsParseAWellFormedCommandLine(t *testing.T) {
	c, err := parseFlags([]string{
		"-url", "ws://127.0.0.1:7880", "-token", "jwt", "-base-key-file", keyFile(t, keyHex+"\n", 0o600), "-leaf", "1", "-epoch", "7",
		"-roster", "0:" + devA + ",1:" + devB, "-publish", "opus,vp8,h264", "-subscribe", "-expect-device", devA,
		"-duration", "3s", "-media", "/m",
	}, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if c.Leaf != 1 || c.Epoch != 7 || c.MinEpoch != 7 || !c.Subscribe || c.Duration != 3*time.Second || c.MediaDir != "/m" {
		t.Fatalf("config = %+v", c)
	}
	if strings.Join(c.Publish, ",") != "opus,vp8,h264" {
		t.Fatalf("publish = %v", c.Publish)
	}
	if len(c.Roster) != 2 || c.Roster[1].Leaf != 1 || hex.EncodeToString(c.Roster[1].Device[:]) != devB {
		t.Fatalf("roster = %+v", c.Roster)
	}
	if hex.EncodeToString(c.ExpectDevice[:]) != devA || c.BaseKey[0] != 0x0a {
		t.Fatalf("expect/base key = %x / %x", c.ExpectDevice, c.BaseKey)
	}
}

// RIGS-10: the base key never travels on argv. It comes from stdin ("-") or from a file only its
// owner can read; the old -base-key flag is gone, and no refusal repeats the key.
func TestTheBaseKeyComesFromStdinOrAPrivateFileNeverFromArgv(t *testing.T) {
	base := []string{"-url", "ws://x", "-token", "t", "-leaf", "0", "-epoch", "1", "-roster", "0:" + devA}
	c, err := parseFlags(append([]string{"-base-key-file", "-"}, base...), strings.NewReader(keyHex+"\n"), &bytes.Buffer{})
	if err != nil || c.BaseKey[15] != 0x0a {
		t.Fatalf("key from stdin: %x, %v", c.BaseKey, err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{"the old flag", append([]string{"-base-key", keyHex}, base...), "", "flag provided but not defined: -base-key"},
		{"no key", base, "", "-base-key-file is required"},
		{"a group-readable file", append([]string{"-base-key-file", keyFile(t, keyHex, 0o640)}, base...), "", "must be readable by its owner only"},
		{"a world-readable file", append([]string{"-base-key-file", keyFile(t, keyHex, 0o604)}, base...), "", "must be readable by its owner only"},
		{"a 0644 file", append([]string{"-base-key-file", keyFile(t, keyHex, 0o644)}, base...), "", "must be readable by its owner only"},
		{"stdin closed early", append([]string{"-base-key-file", "-"}, base...), keyHex[:16], "32 lowercase hex"},
		{"uppercase on stdin", append([]string{"-base-key-file", "-"}, base...), strings.ToUpper(keyHex), "32 lowercase hex"},
		{"short key in a file", append([]string{"-base-key-file", keyFile(t, "0a0a", 0o600)}, base...), "", "32 lowercase hex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(tc.args, strings.NewReader(tc.stdin), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseFlags = %v, want an error containing %q", err, tc.want)
			}
			if strings.Contains(strings.ToLower(err.Error()), keyHex) {
				t.Fatalf("the refusal repeats the key: %v", err)
			}
		})
	}
}

func TestKeyFileRejectsSymlink(t *testing.T) {
	target := keyFile(t, keyHex, 0o600)
	link := filepath.Join(t.TempDir(), "key-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readBaseKey(link, strings.NewReader("")); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestKeyFileRejectsAnotherOwnerWhenConstructible(t *testing.T) {
	path := keyFile(t, keyHex, 0o600)
	other := os.Geteuid() + 1
	if err := os.Chown(path, other, -1); err != nil {
		t.Skipf("cannot construct a file owned by another uid: %v", err)
	}
	if _, err := readBaseKey(path, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "owned by the current user") {
		t.Fatalf("other owner's key file: %v", err)
	}
}

func TestRunClearsTheCallerKeyAfterBuildingBothCipherSides(t *testing.T) {
	c := config{BaseKey: [16]byte{1}, Leaf: 0, Epoch: 1, MinEpoch: 1, Roster: []sframe.RosterEntry{{Leaf: 0, Device: [16]byte{1}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = run(ctx, &c)
	if c.BaseKey != [16]byte{} {
		t.Fatalf("caller key still present: %x", c.BaseKey)
	}
}

func TestFlagsRefuseWhatTheBotCannotUse(t *testing.T) {
	kf := keyFile(t, keyHex, 0o600)
	base := []string{"-url", "ws://x", "-token", "t", "-base-key-file", kf, "-leaf", "0", "-epoch", "1", "-roster", "0:" + devA}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no url", []string{"-token", "t", "-base-key-file", kf, "-leaf", "0", "-roster", "0:" + devA}, "-url and -token are required"},
		{"short key", append(base[:4:4], "-base-key-file", keyFile(t, "0a0a", 0o600), "-leaf", "0", "-roster", "0:"+devA), "-base-key-file must hold the base key as 32 lowercase hex"},
		{"leaf range", append(append([]string{}, base...), "-leaf", "65536"), "-leaf must be 0…65535"},
		{"bad roster", append(append([]string{}, base...), "-roster", "x:"+devA), `-roster entry "x:` + devA + `"`},
		{"unknown codec", append(append([]string{}, base...), "-publish", "av1"), `-publish: unknown "av1"`},
		{"subscribe without device", append(append([]string{}, base...), "-subscribe"), "-subscribe needs -expect-device"},
		{"min epoch above epoch", append(append([]string{}, base...), "-min-epoch", "2"), "-min-epoch must not exceed -epoch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(tc.args, strings.NewReader(""), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseFlags(%v) = %v, want an error containing %q", tc.args, err, tc.want)
			}
		})
	}
}

// One bot publishes Opus, VP8 and H.264 through the real in-process LiveKit; the other decrypts
// them. The ports come from sfutest.FreePorts: every LiveKit-booting package test runs in its own
// process, concurrently with the others under `go test ./...`, and only internal/sfu uses fixed ports.
func TestTwoBotsDecryptEachOtherThroughTheSFU(t *testing.T) {
	if raceEnabled {
		t.Skip("upstream livekit-server data race in updateRidsFromSDP; runs in the non-race step")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg := sfu.DefaultConfig()
	cfg.Port, cfg.UDPPort = sfutest.FreePorts(t)
	cfg.APISecret = strings.Repeat("m", 32)
	cfg.STUNServers = []string{"127.0.0.1:3478"} // RIGS-06: no third-party STUN host from a test
	srv, err := sfu.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("sfu.Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	const room = "mediabot-loopback"
	if err := srv.CreateRoom(ctx, room); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	tokA, err := srv.Token(room, devA, sfu.PublishGrant(true, true, true), nil)
	if err != nil {
		t.Fatalf("Token A: %v", err)
	}
	tokB, err := srv.Token(room, devB, sfu.PublishGrant(true, true, true), nil)
	if err != nil {
		t.Fatalf("Token B: %v", err)
	}
	a, _ := deviceID(devA)
	b, _ := deviceID(devB)
	var key [16]byte
	for i := range key {
		key[i] = 0x0a
	}
	common := config{
		URL: srv.URL(), BaseKey: key, Epoch: 1, MinEpoch: 1,
		Roster:   []sframe.RosterEntry{{Leaf: 0, Device: a}, {Leaf: 1, Device: b}},
		MediaDir: filepath.Join("..", "..", "internal", "media", "testdata"),
	}
	sub := common
	sub.Token, sub.Leaf, sub.Subscribe, sub.ExpectDevice, sub.Duration = tokB, 1, true, a, 12*time.Second
	pub := common
	pub.Token, pub.Leaf, pub.Publish, pub.Duration = tokA, 0, []string{"opus", "vp8", "h264"}, 8*time.Second

	var wg sync.WaitGroup
	var subRep report
	var subErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		subRep, subErr = run(ctx, &sub)
	}()
	time.Sleep(time.Second)
	pubRep, err := run(ctx, &pub)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	wg.Wait()
	if subErr != nil {
		t.Fatalf("subscriber: %v", subErr)
	}
	for _, what := range []string{"opus", "vp8", "h264"} {
		if !strings.HasPrefix(pubRep.Published[what], "TR_") {
			t.Errorf("published[%s] = %q, want a track sid", what, pubRep.Published[what])
		}
	}
	if subRep.Decrypted == 0 {
		t.Fatalf("the subscriber decrypted nothing: %+v", subRep)
	}
	for code, n := range subRep.Dropped {
		if code != "sif" && n > 0 {
			t.Errorf("dropped[%s] = %d; only SFU-injected SIF frames may be dropped", code, n)
		}
	}
}
