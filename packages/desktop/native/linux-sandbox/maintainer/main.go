//go:build linux

// The privileged maintainer has no GUI, network or caller-selected namespace.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jonasthim/dilla/internal/desktopsandbox"
)

// Set only by the reviewed build step through -ldflags. No environment or CLI
// option changes this public trust. An unconfigured binary fails before IO.
var publicConfiguration string

type compiledConfiguration struct {
	PublicKeys  []string                 `json:"publicKeys"`
	Generation  int64                    `json:"generation"`
	PublishedAt string                   `json:"publishedAt"`
	ExpiresAt   string                   `json:"expiresAt"`
	Helper      desktopsandbox.Component `json:"helper"`
}

func configuration() (desktopsandbox.Trust, error) {
	if len(publicConfiguration) == 0 || len(publicConfiguration) > 16384 {
		return desktopsandbox.Trust{}, desktopsandbox.ErrAuthentication
	}
	raw, e := base64.StdEncoding.Strict().DecodeString(publicConfiguration)
	if e != nil {
		return desktopsandbox.Trust{}, desktopsandbox.ErrAuthentication
	}
	var c compiledConfiguration
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || len(c.PublicKeys) < 1 || len(c.PublicKeys) > 8 {
		return desktopsandbox.Trust{}, desktopsandbox.ErrAuthentication
	}
	trust := desktopsandbox.Trust{Initial: &desktopsandbox.InitialTrust{Generation: c.Generation, CompatibilityEpoch: 1, SandboxAPI: 1, PublishedAt: c.PublishedAt, ExpiresAt: c.ExpiresAt, Helper: c.Helper}}
	for _, encoded := range c.PublicKeys {
		k, e := base64.StdEncoding.Strict().DecodeString(encoded)
		if e != nil || len(k) != ed25519.PublicKeySize {
			return desktopsandbox.Trust{}, desktopsandbox.ErrAuthentication
		}
		trust.PublicKeys = append(trust.PublicKeys, ed25519.PublicKey(k))
	}
	return trust, nil
}

// Capture before opening config, root files or establishing timers/netpoll.
// Normal launchers close unintended inherited FDs. Only EBADF means no handoff.
func inheritedLock() (bool, error) {
	var s unix.Stat_t
	e := unix.Fstat(3, &s)
	if errors.Is(e, unix.EBADF) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return true, nil
}
func main() {
	handoff, e := inheritedLock()
	if e != nil {
		os.Exit(2)
	}
	code := run(os.Args[1:], handoff, os.Stdout)
	if code != 0 {
		_, _ = os.Stderr.WriteString("Dilla sandbox maintenance requires a safe authorized retry.\n")
	}
	os.Exit(code)
}
func run(args []string, handoff bool, out io.Writer) int {
	role := "install"
	if len(args) == 1 && (args[0] == "--check" || args[0] == "--remove") {
		role = args[0]
	} else if len(args) != 0 {
		return 2
	}
	trust, e := configuration()
	if e != nil {
		return 2
	}
	if role == "--check" {
		if json.NewEncoder(out).Encode(desktopsandbox.ReadAppImageStatus(trust)) != nil {
			return 2
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m, e := desktopsandbox.OpenAppImageMaintainer(ctx, trust, handoff)
	if e != nil {
		return 2
	}
	defer func() { _ = m.Close() }()
	if role == "--remove" {
		e = m.Remove(ctx)
	} else {
		_, e = m.Process(ctx, &boundedStdin{ctx: ctx, fd: 0})
	}
	if e != nil {
		return 2
	}
	return 0
}

// poll bounds an unclosed pipe as well as input arriving in partial frames.
// No goroutine keeps reading attacker-controlled stdin after the operation ends.
type boundedStdin struct {
	ctx context.Context
	fd  int
}

func (r *boundedStdin) Read(b []byte) (int, error) {
	if r.fd < 0 || r.fd > math.MaxInt32 {
		return 0, desktopsandbox.ErrState
	}
	if len(b) == 0 {
		return 0, nil
	}
	for {
		if e := r.ctx.Err(); e != nil {
			return 0, e
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN | unix.POLLHUP}}
		n, e := unix.Poll(fds, 25)
		if errors.Is(e, unix.EINTR) {
			continue
		}
		if e != nil {
			return 0, e
		}
		if n == 0 {
			continue
		}
		count, e := unix.Read(r.fd, b)
		if errors.Is(e, unix.EINTR) || errors.Is(e, unix.EAGAIN) {
			continue
		}
		if e != nil {
			return 0, e
		}
		if count == 0 {
			return 0, io.EOF
		}
		return count, nil
	}
}
