//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/id"
)

// The supervised Bot (Bot v2 task 11). dillad's supervisor starts dillabot through `dillad bot-launch` with no
// arguments, an environment of exactly PATH, HOME, DILLA_BOT_SOCKET, DILLA_BOT_STATE_DIR, DILLA_BOT_HELPER and
// DILLA_BOT_WASI, its state directory as cwd, and the launch token on fd 3. It reaches dillad only over the Unix socket
// DILLA_BOT_SOCKET: its gateway connections (the first identify carries the launch token), which carry every request
// the Bot's device session makes as request frames, and the few HTTP routes it calls before its gateway exists
// (discovery, its enrollment, its session and its device list), the only HTTP routes dillad serves beside /gateway on
// that socket (dillad.BotSocketHTTPRoutes). There is no base_url and no network path.
//
// There is no configuration file (Bot v2 task 14): the one file the Bot reads from its state directory is
// runtime.token, the runtime credential (0600, the Bot's). Its client rate limits are built-in defaults
// (client.RateLimits), and it has no reconcile interval: it polls nothing.
const (
	launchTokenFD        = 3
	supervisedCredential = "runtime.token"
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
		Socket:           os.Getenv("DILLA_BOT_SOCKET"),
		StateDir:         os.Getenv("DILLA_BOT_STATE_DIR"),
		HelperExecutable: os.Getenv("DILLA_BOT_HELPER"),
		WASIModule:       os.Getenv("DILLA_BOT_WASI"),
		launch:           launch,
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
	credential, err := readCredential(c.RuntimeCredentialFile)
	if err != nil {
		return config{}, errCredentialBad
	}
	clear(credential)
	c.InstanceID, err = discoverInstanceID(ctx, c.Socket)
	if err != nil {
		return config{}, fmt.Errorf("instance discovery over %s: %w", c.Socket, err)
	}
	// c.RateLimits stays zero: the built-in defaults, also under dev_same_user and in the e2e test host (fix round 1,
	// I-3: the Bot spaces its passes, so it fits the production meters without a tuning of its own).
	return c, nil
}

// discoverInstanceID reads the instance id from GET /v1/instance over the socket. The socket is dillad's own (it sits
// in dillad's runtime directory, and dillad admitted this very process on it), so its answer is the instance's.
func discoverInstanceID(ctx context.Context, socket string) (id.ID, error) {
	transport := unixTransport(socket)
	defer transport.CloseIdleConnections()
	return discoverInstance(ctx, &http.Client{Transport: transport}, supervisedOrigin)
}

// discoverInstance reads the instance id from origin's GET /v1/instance.
func discoverInstance(ctx context.Context, hc *http.Client, origin string) (id.ID, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/v1/instance", nil) // #nosec G704 -- origin is the fixed socket origin or the operator's --remote flag
	if err != nil {
		return id.ID{}, err
	}
	req.Header.Set("Accept", "application/cbor")
	res, err := hc.Do(req) // #nosec G704 -- as above: the request goes only to the configured instance
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
