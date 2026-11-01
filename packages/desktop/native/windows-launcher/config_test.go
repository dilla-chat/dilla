package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestBuildConfigRejectsMutableTrustOrUnsafeIdentity(t *testing.T) {
	cfg, _ := initialInstallation(t)
	raw, _ := json.Marshal(cfg)
	if _, e := parseBuildConfig(base64.StdEncoding.EncodeToString(raw)); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*buildConfig){func(c *buildConfig) { c.ProductName = "../other" }, func(c *buildConfig) { c.ProductName = "Dilla." }, func(c *buildConfig) { c.ProductName = "CON" }, func(c *buildConfig) { c.Channel = "other" }, func(c *buildConfig) { c.InitialManifestSHA512 = "wrong" }, func(c *buildConfig) { c.PublicKeys = []string{"untrusted malformed key"} }, func(c *buildConfig) { c.AppID = "invalid/app" }} {
		candidate := cfg
		mutate(&candidate)
		bytes, _ := json.Marshal(candidate)
		if _, e := parseBuildConfig(base64.StdEncoding.EncodeToString(bytes)); e == nil {
			t.Fatalf("accepted invalid config %+v", candidate)
		}
	}
}

func TestOrdinaryEnvironmentDropsBootstrapAndElectronInjection(t *testing.T) {
	env := ordinaryEnvironment([]string{"Path=C:/Windows", "DILLA_UPDATE_NONCE=secret", "dilla_update_previous_pid=123", "ELECTRON_RUN_AS_NODE=1", "NODE_OPTIONS=--require evil", "HOME=unchanged"})
	if len(env) != 2 || env[0] != "Path=C:/Windows" || env[1] != "HOME=unchanged" {
		t.Fatalf("unsafe environment forwarded: %q", env)
	}
}
