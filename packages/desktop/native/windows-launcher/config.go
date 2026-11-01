package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

var productPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._ -]{0,63}$`)
var appIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.-]{0,127}$`)

func parseBuildConfig(encoded string) (buildConfig, error) {
	var cfg buildConfig
	if len(encoded) > 131072 {
		return cfg, errors.New("compiled configuration exceeds bound")
	}
	raw, e := base64.StdEncoding.Strict().DecodeString(encoded)
	if e != nil {
		return cfg, e
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&cfg); e != nil {
		return cfg, e
	}
	if e = d.Decode(new(any)); !errors.Is(e, io.EOF) {
		return cfg, errors.New("invalid compiled configuration")
	}
	name := cfg.ProductName
	stem := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " "))
	reserved := stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9'
	if cfg.Format != 1 || !productPattern.MatchString(name) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || reserved || !appIDPattern.MatchString(cfg.AppID) || strings.HasSuffix(cfg.AppID, ".") || cfg.Arch != "x64" || len(cfg.PublicKeys) < 1 || len(cfg.PublicKeys) > 8 {
		return cfg, errors.New("invalid compiled identity")
	}
	for _, key := range cfg.PublicKeys {
		block, rest := pem.Decode([]byte(key))
		if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
			return cfg, errors.New("invalid compiled public key")
		}
		parsed, e := x509.ParsePKIXPublicKey(block.Bytes)
		if e != nil {
			return cfg, e
		}
		if _, ok := parsed.(ed25519.PublicKey); !ok {
			return cfg, errors.New("compiled release key must be Ed25519")
		}
	}
	slot, e := json.Marshal(desktoplaunch.Slot{Format: 1, Kind: "initial", Version: cfg.InitialVersion, ManifestSHA512: cfg.InitialManifestSHA512})
	if e != nil {
		return cfg, e
	}
	if _, e = desktoplaunch.VerifySlot(slot, cfg.trust(), "", time.Now()); e != nil {
		return cfg, e
	}
	return cfg, nil
}

func ordinaryEnvironment(env []string) []string {
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
		if strings.HasPrefix(key, "DILLA_UPDATE_") || strings.HasPrefix(key, "DILLA_DESKTOP_") || key == "ELECTRON_RUN_AS_NODE" || key == "NODE_OPTIONS" || key == "NODE_PATH" {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}
