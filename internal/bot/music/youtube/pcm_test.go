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

func TestPCMStarterFailsClosedWithoutBubblewrap(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "invoked")
	ytdlp := writeScript(t, dir, "yt-dlp", `touch "$MARKER"; printf encoded`)
	ffmpeg := writeScript(t, dir, "ffmpeg", `touch "$MARKER"; cat`)
	t.Setenv("MARKER", marker)

	pcm, err := (PCMStarter{YTDLP: ytdlp, FFmpeg: ffmpeg, Bubblewrap: filepath.Join(dir, "missing-bwrap")}).Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if pcm != nil || !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("Start = (%v, %v), want nil and ErrSandboxUnavailable", pcm, err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unsandboxed subprocess started: stat error = %v", statErr)
	}
}

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

func TestPCMStarterExecutableResolvesBareNameAndRejectsDirectory(t *testing.T) {
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
	starter := PCMStarter{}
	resolved, err := starter.executable("ffmpeg-test", "ffmpeg")
	if err != nil || resolved != target {
		t.Fatalf("bare executable resolution = (%q, %v), want (%q, nil)", resolved, err, target)
	}
	if _, err := starter.executable("directory-tool", "ffmpeg"); err == nil {
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

func TestPCMChildCannotReachPublicNetworkDirectly(t *testing.T) {
	dir := t.TempDir()
	ytdlp := filepath.Join(dir, "direct-network-probe")
	program := "#!/usr/bin/python3\nimport socket,sys\ntry:\n socket.create_connection(('8.8.8.8',443),0.5)\n sys.exit(41)\nexcept OSError:\n sys.stdout.write('encoded')\n"
	if err := os.WriteFile(ytdlp, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	ffmpeg := writeScript(t, dir, "fake ffmpeg", `cat`)
	source, err := (PCMStarter{YTDLP: ytdlp, FFmpeg: ffmpeg, Timeout: 5 * time.Second}).Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer source.Close()
	pcm, err := io.ReadAll(source)
	if err != nil || string(pcm) != "encoded" {
		t.Fatalf("network-isolated output = %q, %v; fake extractor should see direct egress denied", pcm, err)
	}
}

func TestPCMStarterRunsFakeExtractorAndFFmpegInsideIsolatedWorker(t *testing.T) {
	dir := t.TempDir()
	ytdlp := writeScript(t, dir, "fake yt-dlp", `test "$HTTPS_PROXY" = "http://127.0.0.1:3128" || exit 31; printf encoded`)
	ffmpeg := writeScript(t, dir, "fake ffmpeg", `test "$HTTPS_PROXY" = "http://127.0.0.1:3128" || exit 32; cat`)
	starter := PCMStarter{YTDLP: ytdlp, FFmpeg: ffmpeg, Timeout: 5 * time.Second}
	source, err := starter.Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer source.Close()
	pcm, err := io.ReadAll(source)
	if err != nil || string(pcm) != "encoded" {
		t.Fatalf("PCM output = %q, %v, want encoded", pcm, err)
	}
}

func TestPCMStarterStopsAtConfiguredOutputLimit(t *testing.T) {
	dir := t.TempDir()
	ytdlp := writeScript(t, dir, "fake yt-dlp", `printf encoded`)
	ffmpeg := writeScript(t, dir, "fake ffmpeg", `cat`)
	source, err := (PCMStarter{YTDLP: ytdlp, FFmpeg: ffmpeg, MaxPCMBytes: 4, Timeout: 5 * time.Second}).Start(context.Background(), "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer source.Close()
	pcm, err := io.ReadAll(source)
	if !errors.Is(err, ErrPCMLimit) || string(pcm) != "enco" {
		t.Fatalf("bounded PCM = %q, %v; want 4 bytes and ErrPCMLimit", pcm, err)
	}
}

func TestPCMStarterCancellationStopsTheSandboxedPipeline(t *testing.T) {
	dir := t.TempDir()
	ytdlp := writeScript(t, dir, "fake yt-dlp", `while :; do sleep 1; done`)
	ffmpeg := writeScript(t, dir, "fake ffmpeg", `cat`)
	ctx, cancel := context.WithCancel(context.Background())
	source, err := (PCMStarter{YTDLP: ytdlp, FFmpeg: ffmpeg, MaxDuration: time.Minute, Timeout: 5 * time.Second}).Start(ctx, "https://www.youtube.com/watch?v=aqz-KE-bpKQ")
	if err != nil {
		if errors.Is(err, ErrSandboxUnavailable) {
			t.Skipf("bubblewrap network namespaces unavailable: %v", err)
		}
		t.Fatal(err)
	}
	cancel()
	readDone := make(chan error, 1)
	go func() { _, readErr := source.Read(make([]byte, 8)); readDone <- readErr }()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		_ = source.Close()
		t.Fatal("sandboxed pipeline did not stop after context cancellation")
	}
	_ = source.Close()
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

func TestPCMStarterRejectsAnythingButCanonicalVideoBeforeStartingProcesses(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "invoked")
	ytdlp := writeScript(t, t.TempDir(), "yt-dlp", `touch "$MARKER"`)
	t.Setenv("MARKER", marker)
	for _, input := range []string{"ytsearch5:hello", "https://example.org/video", "file:///tmp/audio", "https://youtube.com/playlist?list=PLabc_123"} {
		if _, err := (PCMStarter{YTDLP: ytdlp, FFmpeg: ytdlp}).Start(context.Background(), input); err == nil {
			t.Errorf("Start(%q) succeeded", input)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("yt-dlp invoked for invalid input, stat error = %v", err)
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

func TestBoundedPCMReaderStopsAtByteLimit(t *testing.T) {
	r, err := NewBoundedPCMReader(context.Background(), io.NopCloser(strings.NewReader("abcdef")), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if !errors.Is(err, ErrPCMTooLarge) || string(got) != "abcd" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
}

func TestBoundedPCMReaderAllowsEOFAtLimit(t *testing.T) {
	r, err := NewBoundedPCMReader(context.Background(), io.NopCloser(strings.NewReader("abcd")), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "abcd" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
}

func TestBoundedPCMReaderCancellationClosesBlockedSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &blockingPCMSource{closed: make(chan struct{})}
	r, err := NewBoundedPCMReader(ctx, source, 1024)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() { _, readErr := r.Read(make([]byte, 8)); readDone <- readErr }()
	cancel()
	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read remained blocked after cancellation")
	}
	select {
	case <-source.closed:
	case <-time.After(time.Second):
		t.Fatal("source was not closed after cancellation")
	}
	_ = r.Close()
}

type blockingPCMSource struct{ closed chan struct{} }

func (s *blockingPCMSource) Read([]byte) (int, error) {
	<-s.closed
	return 0, errors.New("source closed")
}
func (s *blockingPCMSource) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}
