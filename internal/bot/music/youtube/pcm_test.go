package youtube

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxTempDirUsesConfiguredTMPDIRWithPrivatePermissions(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	root, err := newSandboxTempDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(root) != parent {
		t.Fatalf("sandbox temp dir parent = %q, want configured TMPDIR %q", filepath.Dir(root), parent)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("sandbox temp dir permissions = %04o, want 0700", info.Mode().Perm())
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sandbox temp dir cleanup stat error = %v, want not-exist", err)
	}
}

func TestSandboxTempDirUsesDefaultTempDirectoryWhenTMPDIRIsUnset(t *testing.T) {
	t.Setenv("TMPDIR", "")
	defaultDir := os.TempDir()
	probe, err := os.CreateTemp(defaultDir, "dilla-yt-probe-")
	if err != nil {
		t.Skipf("default temp directory is not writable in this environment: %v", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(probePath); err != nil {
		t.Fatal(err)
	}

	root, err := newSandboxTempDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(root) != defaultDir {
		t.Fatalf("sandbox temp dir parent = %q, want default temp dir %q", filepath.Dir(root), defaultDir)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
}

func TestLookupToolResolvesBareNameAndRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	target := writeScript(t, dir, "ffmpeg-test", `exit 0`)
	directory := filepath.Join(dir, "directory-tool")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	resolved, err := lookupTool("ffmpeg-test", "ffmpeg")
	if err != nil || resolved != target {
		t.Fatalf("bare executable resolution = (%q, %v), want (%q, nil)", resolved, err, target)
	}
	if _, err := lookupTool("directory-tool", "ffmpeg"); err == nil {
		t.Fatal("directory accepted as an executable")
	}
}

func TestSandboxCommandPointsTLSAtMountedResolvedCABundle(t *testing.T) {
	if !strings.Contains(sandboxWorker, `"SSL_CERT_FILE": "/etc/ssl/cert.pem"`) {
		t.Fatal("embedded worker does not point child tools at the mounted resolved CA bundle")
	}
	cmd, stdout, err := sandboxCommand(context.Background(), "/usr/bin/bwrap", "/usr/bin/python3", nil, nil, "/tmp/youtube-test/e.sock")
	if err != nil {
		t.Fatal(err)
	}
	_ = stdout.Close()
	args := cmd.Args
	caBundle, _, err := systemCABundle()
	if err != nil {
		t.Fatal(err)
	}
	bundleMounted := false
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == caBundle && args[i+2] == "/etc/ssl/cert.pem" {
			bundleMounted = true
			break
		}
	}
	if !bundleMounted {
		t.Fatalf("sandbox command does not mount resolved CA bundle %q at /etc/ssl/cert.pem", caBundle)
	}
	envFound := false
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == "SSL_CERT_FILE" {
			if args[i+2] != "/etc/ssl/cert.pem" {
				t.Fatalf("SSL_CERT_FILE = %q, want mounted resolved bundle /etc/ssl/cert.pem", args[i+2])
			}
			envFound = true
			break
		}
	}
	if !envFound {
		t.Fatal("sandbox command does not set SSL_CERT_FILE")
	}
	if info, err := os.Stat("/etc/alternatives"); err == nil && info.IsDir() {
		mounted := false
		for i := 0; i+2 < len(args); i++ {
			if args[i] == "--ro-bind" && args[i+1] == "/etc/alternatives" && args[i+2] == "/etc/alternatives" {
				mounted = true
				break
			}
		}
		if !mounted {
			t.Fatal("sandbox command does not mount FFmpeg's alternative library symlinks")
		}
	}
}

func TestEgressBrokerRejectsNonYouTubeAndPrivateDestinations(t *testing.T) {
	for _, host := range []string{"example.com", "youtube.com.evil.test", "127.0.0.1", "[::1]", "..youtube.com", "a..youtube.com"} {
		if allowedYouTubeHost(host) {
			t.Errorf("allowedYouTubeHost(%q) = true", host)
		}
	}
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "100.64.0.1", "192.88.99.1", "::1", "fd00::1", "fe80::1", "2001:db8::1", "::ffff:127.0.0.1"} {
		if publicEgressAddress(address) {
			t.Errorf("publicEgressAddress(%q) = true", address)
		}
	}
	if !publicEgressAddress("8.8.8.8") || !publicEgressAddress("2606:4700:4700::1111") {
		t.Fatal("public addresses were rejected")
	}
	for _, header := range []string{
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"CONNECT video.googlevideo.com:80 HTTP/1.1\r\nHost: video.googlevideo.com\r\n\r\n",
		"GET https://youtube.com/ HTTP/1.1\r\nHost: youtube.com\r\n\r\n",
		"CONNECT youtube.com.evil.test:443 HTTP/1.1\r\nHost: youtube.com.evil.test\r\n\r\n",
		"CONNECT youtube.com:443 HTTP/1.1\r\nHost: youtube.com\r\nX-Forwarded-For: 127.0.0.1\r\n\r\n",
	} {
		if _, err := parseConnectHeader([]byte(header)); err == nil {
			t.Errorf("parseConnectHeader accepted %q", header)
		}
	}
	for _, host := range []string{"youtube.com", "www.youtube.com", "video.googlevideo.com", "i.ytimg.com", "youtu.be"} {
		if !allowedYouTubeHost(host) {
			t.Errorf("allowedYouTubeHost(%q) = false", host)
		}
	}
}

func TestEgressBrokerReturns403ForDisallowedCONNECTWithoutDialing(t *testing.T) {
	client, brokerConn := net.Pipe()
	defer client.Close()
	defer brokerConn.Close()
	broker := &egressBroker{}
	done := make(chan struct{})
	go func() {
		broker.handle(brokerConn)
		close(done)
	}()
	if _, err := io.WriteString(client, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 64)
	n, err := client.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(response[:n]), "HTTP/1.1 403 Forbidden\r\n") {
		t.Fatalf("broker response = %q, want 403", response[:n])
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broker did not finish rejected request")
	}
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
