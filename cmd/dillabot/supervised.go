//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/pelletier/go-toml/v2"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/id"
)

// The supervised Bot (Bot v2 task 11). dillad's supervisor starts dillabot through `dillad bot-launch` with no
// arguments, an environment of exactly PATH, HOME, DILLA_BOT_SOCKET, DILLA_BOT_STATE_DIR, DILLA_BOT_HELPER and
// DILLA_BOT_WASI, its state directory as cwd, and the launch token on fd 3. It reaches dillad only over the Unix socket
// DILLA_BOT_SOCKET: its gateway connections (the first identify carries the launch token) and, until Task 14 moves
// them onto the gateway, its HTTP calls too, which dillad serves on the same socket. There is no base_url and no
// network path.
//
// Two files in the state directory stand in for what bot.toml held until Task 14 removes it: runtime.token, the
// runtime credential (0600, the Bot's), and an optional bot.toml with only reconcile_interval and [rate_limits].
const (
	launchTokenFD           = 3
	supervisedCredential    = "runtime.token"
	supervisedTuningFile    = "bot.toml"
	supervisedReconcileTime = 30 * time.Second
	// supervisedOrigin is the HTTP origin of the socket's requests; the host name is never resolved.
	supervisedOrigin = "http://localhost"
)

// supervisedMode reports whether dillad's supervisor started this process.
func supervisedMode() bool { return len(os.Args) == 1 && os.Getenv("DILLA_BOT_SOCKET") != "" }

// readLaunchToken reads the 32-byte launch token from fd 3 and closes it. The token lives in memory only.
func readLaunchToken() ([]byte, error) {
	f := os.NewFile(launchTokenFD, "launch-token")
	if f == nil {
		return nil, errors.New("no launch token on fd 3")
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 33)
	n, err := io.ReadFull(f, buf)
	if n != 32 || !errors.Is(err, io.ErrUnexpectedEOF) {
		clear(buf)
		return nil, errors.New("fd 3 does not carry exactly a 32-byte launch token")
	}
	return buf[:32], nil
}

// unixTransport dials dillad's Bot socket for every request, whatever the URL's host.
func unixTransport(socket string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
}

// loadSupervisedConfig builds the configuration of a supervised Bot from its environment, its state directory and the
// instance's own discovery document, read over the socket.
func loadSupervisedConfig(ctx context.Context) (config, error) {
	token, err := readLaunchToken()
	if err != nil {
		return config{}, err
	}
	launch, err := gatewayclient.NewLaunchToken(token)
	if err != nil {
		return config{}, err
	}
	c := config{
		BaseURL: supervisedOrigin, AllowLoopbackHTTP: true,
		Socket:            os.Getenv("DILLA_BOT_SOCKET"),
		StateDir:          os.Getenv("DILLA_BOT_STATE_DIR"),
		HelperExecutable:  os.Getenv("DILLA_BOT_HELPER"),
		WASIModule:        os.Getenv("DILLA_BOT_WASI"),
		ReconcileInterval: supervisedReconcileTime,
		launch:            launch,
	}
	for name, value := range map[string]string{
		"DILLA_BOT_SOCKET": c.Socket, "DILLA_BOT_STATE_DIR": c.StateDir,
		"DILLA_BOT_HELPER": c.HelperExecutable, "DILLA_BOT_WASI": c.WASIModule,
	} {
		if !filepath.IsAbs(value) {
			return config{}, fmt.Errorf("%w: %s must be an absolute path", errConfigInvalid, name)
		}
	}
	c.RuntimeCredentialFile = filepath.Join(c.StateDir, supervisedCredential)
	if err := c.loadTuning(filepath.Join(c.StateDir, supervisedTuningFile)); err != nil {
		return config{}, err
	}
	if err := c.RateLimits.Validate(); err != nil {
		return config{}, fmt.Errorf("%w: rate_limits requires positive finite rates and bounded cache capacities", errConfigInvalid)
	}
	credential, err := readCredential(c.RuntimeCredentialFile)
	if err != nil {
		return config{}, errCredentialBad
	}
	clear(credential)
	c.InstanceID, err = discoverInstanceID(ctx, c.Socket)
	if err != nil {
		return config{}, fmt.Errorf("instance discovery over %s: %w", c.Socket, err)
	}
	return c, nil
}

// loadTuning reads the optional tuning file: the Bot's own, owner-only, and holding only reconcile_interval and
// [rate_limits] (strict).
func (c *config) loadTuning(path string) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return errConfigUnreadable
	}
	f := os.NewFile(uintptr(fd), "bot-tuning")
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return errConfigUnreadable
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int64(st.Uid) != int64(os.Geteuid()) || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s must be an owner-only regular file", errConfigInvalid, path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil || len(raw) > maxConfigBytes {
		return errConfigUnreadable
	}
	full := struct {
		RateLimits        client.RateLimits `toml:"rate_limits"`
		ReconcileInterval string            `toml:"reconcile_interval"`
	}{RateLimits: c.RateLimits}
	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&full); err != nil {
		return fmt.Errorf("%w: %s: only reconcile_interval and [rate_limits] are read", errConfigInvalid, path)
	}
	c.RateLimits = full.RateLimits
	if full.ReconcileInterval != "" {
		d, err := time.ParseDuration(full.ReconcileInterval)
		if err != nil || d < time.Second || d > 24*time.Hour {
			return fmt.Errorf("%w: reconcile_interval must be between 1s and 24h", errConfigInvalid)
		}
		c.ReconcileInterval = d
	}
	return nil
}

// discoverInstanceID reads the instance id from GET /v1/instance over the socket. The socket is dillad's own (it sits
// in dillad's runtime directory, and dillad admitted this very process on it), so its answer is the instance's.
func discoverInstanceID(ctx context.Context, socket string) (id.ID, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, supervisedOrigin+"/v1/instance", nil)
	if err != nil {
		return id.ID{}, err
	}
	req.Header.Set("Accept", "application/cbor")
	transport := unixTransport(socket)
	defer transport.CloseIdleConnections()
	res, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return id.ID{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return id.ID{}, fmt.Errorf("GET /v1/instance answered %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return id.ID{}, err
	}
	var doc []cbor.RawMessage
	if err := cbor.Unmarshal(body, &doc); err != nil || len(doc) < 4 {
		return id.ID{}, errors.New("malformed discovery document")
	}
	var instance id.ID
	if err := cbor.Unmarshal(doc[3], &instance); err != nil || instance.IsZero() {
		return id.ID{}, errors.New("malformed instance id")
	}
	return instance, nil
}
