//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// The development and test mode (Bot v2 task 14): `dillabot --remote wss://host --runtime-credential-file F
// --state-dir D --helper H --wasi W` runs the Bot against an instance over the network, through its public gateway and
// the HTTP routes the Bot calls before its gateway exists. It is for an instance that runs no supervised Bot: a dillad
// with a supervisor refuses a managed Bot session on its public gateway (gateway.botOffSocket), so this mode cannot
// stand in for the supervised Bot. ws:// is accepted for a loopback host only. There is no configuration file: these
// flags are the whole configuration, and the client rate limits are built-in defaults.

// remoteFlags parses the dev mode's flags.
func remoteFlags(args []string, stderr io.Writer) (remote, credential, stateDir, helperPath, wasi string, err error) {
	fs := flag.NewFlagSet("dillabot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	r := fs.String("remote", "", "the instance's gateway, wss://host[:port] (ws:// for a loopback host only)")
	c := fs.String("runtime-credential-file", "", "the runtime credential (an owner-only 0600 file)")
	s := fs.String("state-dir", "", "the Bot's private state directory")
	h := fs.String("helper", "", "the dilla-bot-core helper executable")
	w := fs.String("wasi", "", "the dilla-core WASI module")
	if err = fs.Parse(args); err != nil {
		return
	}
	if fs.NArg() != 0 {
		err = errors.New("unexpected argument")
		return
	}
	return *r, *c, *s, *h, *w, nil
}

// remoteOrigin is the HTTP origin of a --remote gateway URL, and whether it is a loopback http:// one.
func remoteOrigin(remote string) (origin string, loopback bool, err error) {
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/" && u.Path != "/gateway") {
		return "", false, errors.New("--remote must be wss://host[:port]")
	}
	switch u.Scheme {
	case "wss":
		return "https://" + u.Host, false, nil
	case "ws":
		host := strings.ToLower(u.Hostname())
		if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" {
			return "", false, errors.New("--remote ws:// is for a loopback host only; use wss://")
		}
		return "http://" + u.Host, true, nil
	}
	return "", false, errors.New("--remote must be wss://host[:port]")
}

// loadRemoteConfig builds the dev mode's configuration from its flags and the instance's discovery document.
func loadRemoteConfig(ctx context.Context, args []string, stderr io.Writer) (config, error) {
	remote, credential, stateDir, helperPath, wasi, err := remoteFlags(args, stderr)
	if err != nil {
		return config{}, fmt.Errorf("%w: %w", errConfigInvalid, err)
	}
	origin, loopback, err := remoteOrigin(remote)
	if err != nil {
		return config{}, fmt.Errorf("%w: %w", errConfigInvalid, err)
	}
	c := config{BaseURL: origin, AllowLoopbackHTTP: loopback, RuntimeCredentialFile: credential, StateDir: stateDir,
		HelperExecutable: helperPath, WASIModule: wasi}
	for name, value := range map[string]string{
		"--runtime-credential-file": credential, "--state-dir": stateDir, "--helper": helperPath, "--wasi": wasi,
	} {
		if !filepath.IsAbs(value) {
			return config{}, fmt.Errorf("%w: %s must be an absolute path", errConfigInvalid, name)
		}
	}
	secret, err := readCredential(credential)
	if err != nil {
		return config{}, errCredentialBad
	}
	clear(secret)
	c.InstanceID, err = discoverInstance(ctx, &http.Client{Timeout: 30 * time.Second}, origin)
	if err != nil {
		return config{}, fmt.Errorf("instance discovery at %s: %w", origin, err)
	}
	return c, nil
}
