package youtube

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	egressReady     = "DILLA-EGRESS-READY /1.0\r\n\r\n"
	egressAck       = "DILLA-EGRESS-ACK /1.0\r\n\r\n"
	maxProxyHeader  = 16 << 10
	maxTunnelBytes  = int64(512 << 20)
	maxProcessBytes = int64(1 << 30)
)

var errEgressPolicy = errors.New("youtube: egress policy rejected request")

type egressBroker struct {
	listener  net.Listener
	ready     chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	sem       chan struct{}
	mu        sync.Mutex
	readyOnce sync.Once
	active    map[net.Conn]struct{}
	wg        sync.WaitGroup
	used      int64
	// pool holds spare upstream connections (egress_pool.go); nil dials every CONNECT. owner is this broker's share
	// of it.
	pool  *upstreamPool
	owner string
}

var brokerSerial atomic.Uint64

func newEgressBroker(parent context.Context, listener net.Listener) *egressBroker {
	ctx, cancel := context.WithCancel(parent)
	b := &egressBroker{listener: listener, ready: make(chan struct{}), ctx: ctx, cancel: cancel, sem: make(chan struct{}, 32), active: map[net.Conn]struct{}{}, pool: upstreams,
		owner: "broker:" + strconv.FormatUint(brokerSerial.Add(1), 10)}
	b.wg.Add(1)
	go b.accept()
	return b
}

func (b *egressBroker) accept() {
	defer b.wg.Done()
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		select {
		case b.sem <- struct{}{}:
			b.mu.Lock()
			b.active[conn] = struct{}{}
			b.mu.Unlock()
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				defer func() { <-b.sem; b.mu.Lock(); delete(b.active, conn); b.mu.Unlock(); _ = conn.Close() }()
				b.handle(conn)
			}()
		default:
			_ = conn.Close()
		}
	}
}

func (b *egressBroker) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, maxProxyHeader)
	header, err := readProxyHeader(reader)
	if err != nil {
		return
	}
	if string(header) == egressReady {
		if _, err := io.WriteString(conn, egressAck); err == nil {
			b.readyOnce.Do(func() { close(b.ready) })
		}
		return
	}
	host, err := parseConnectHeader(header)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	// A spare dialed ahead to exactly this host, verified again, or a new dial; either way the next CONNECT to the
	// host finds a spare (Bot v2 Task 19 fix round 3).
	upstream := b.pool.take(host)
	if upstream == nil {
		ctx, cancel := context.WithTimeout(b.ctx, 4*time.Second)
		dialed, err := dialPublicHTTPS(ctx, host)
		cancel()
		if err != nil {
			_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
			return
		}
		upstream = dialed
	}
	b.pool.prime(b.ctx, b.owner, host)
	defer func() { _ = upstream.Close() }()
	_ = conn.SetDeadline(time.Time{})
	_ = upstream.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	bridgeBounded(conn, upstream, b)
}

func (b *egressBroker) close() {
	b.cancel()
	_ = b.listener.Close()
	b.mu.Lock()
	for conn := range b.active {
		_ = conn.Close()
	}
	b.mu.Unlock()
	b.wg.Wait()
}

func readProxyHeader(r *bufio.Reader) ([]byte, error) {
	var header []byte
	for len(header) <= maxProxyHeader {
		part, err := r.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
		header = append(header, part...)
		if len(header) >= 4 && strings.HasSuffix(string(header), "\r\n\r\n") {
			if r.Buffered() != 0 {
				return nil, errEgressPolicy
			}
			return header, nil
		}
	}
	return nil, errEgressPolicy
}

func parseConnectHeader(header []byte) (string, error) {
	if len(header) > maxProxyHeader || !strings.HasSuffix(string(header), "\r\n\r\n") {
		return "", errEgressPolicy
	}
	lines := strings.Split(strings.TrimSuffix(string(header), "\r\n\r\n"), "\r\n")
	if len(lines) == 0 || len(lines) > 34 {
		return "", errEgressPolicy
	}
	request := strings.Split(lines[0], " ")
	if len(request) != 3 || request[0] != "CONNECT" || request[2] != "HTTP/1.0" && request[2] != "HTTP/1.1" {
		return "", errEgressPolicy
	}
	authority := request[1]
	if strings.Count(authority, ":") != 1 || strings.ContainsAny(authority, "@[]%") {
		return "", errEgressPolicy
	}
	parts := strings.SplitN(authority, ":", 2)
	host := strings.ToLower(parts[0])
	if parts[1] != "443" || !allowedYouTubeHost(host) {
		return "", errEgressPolicy
	}
	seen := map[string]bool{}
	for _, line := range lines[1:] {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			return "", errEgressPolicy
		}
		name, value, ok := strings.Cut(line, ":")
		key := strings.ToLower(name)
		if !ok || name == "" || seen[key] || !headerToken(name) {
			return "", errEgressPolicy
		}
		seen[key] = true
		if key != "host" && key != "proxy-connection" && key != "connection" && key != "user-agent" {
			return "", errEgressPolicy
		}
		if key == "host" {
			v := strings.ToLower(strings.TrimSpace(value))
			if v != host && v != host+":443" && v != "127.0.0.1:3128" {
				return "", errEgressPolicy
			}
		}
	}
	return host, nil
}

func headerToken(value string) bool {
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			return false
		}
	}
	return true
}

func allowedYouTubeHost(host string) bool {
	host = strings.ToLower(host)
	if len(host) == 0 || len(host) > 253 || strings.HasSuffix(host, ".") || !hostDomainSyntax(host) {
		return false
	}
	if host == "youtube.com" || host == "youtu.be" {
		return true
	}
	for _, suffix := range []string{"youtube.com", "googlevideo.com", "ytimg.com"} {
		if strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func hostDomainSyntax(host string) bool {
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func publicEgressAddress(raw string) bool {
	addr, err := netip.ParseAddr(raw)
	if err != nil || addr.Is4In6() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	if addr.Is4() {
		for _, prefix := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"} {
			if netip.MustParsePrefix(prefix).Contains(addr) {
				return false
			}
		}
		return true
	}
	for _, prefix := range []string{"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/23", "2001:db8::/32", "2002::/16"} {
		if netip.MustParsePrefix(prefix).Contains(addr) {
			return false
		}
	}
	return true
}

func dialPublicHTTPS(ctx context.Context, host string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	return dialPublicHTTPSWith(ctx, host, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
}
func dialPublicHTTPSWith(ctx context.Context, host string, lookup func(context.Context, string) ([]net.IPAddr, error), dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	dnsMS, dialMS := int64(0), int64(0)
	family := "none"
	failed := 0
	defer func() {
		diagnosticLogger(ctx).DebugContext(ctx, "youtube CONNECT", "host_class", youtubeHostClass(host), "dns_ms", dnsMS, "dial_ms", dialMS, "family", family, "failed_addresses", failed)
	}()
	if !allowedYouTubeHost(host) {
		return nil, errEgressPolicy
	}
	dnsStart := time.Now()
	ips, err := lookup(ctx, host)
	dnsMS = time.Since(dnsStart).Milliseconds()
	if err != nil || len(ips) == 0 || len(ips) > 64 {
		return nil, errEgressPolicy
	}
	addresses := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip.IP)
		if !ok || !publicEgressAddress(addr.String()) {
			return nil, errEgressPolicy
		}
		addresses = append(addresses, addr)
	}
	dialStart := time.Now()
	defer func() { dialMS = time.Since(dialStart).Milliseconds() }()
	for _, addr := range addresses {
		conn, err := dial(ctx, "tcp", net.JoinHostPort(addr.String(), "443"))
		if err == nil {
			family = "ipv6"
			if addr.Is4() {
				family = "ipv4"
			}
			return conn, nil
		}
		failed++
	}
	return nil, errors.New("youtube: egress connection failed")
}

func bridgeBounded(client, upstream net.Conn, broker *egressBroker) {
	var wg sync.WaitGroup
	copyOne := func(dst, src net.Conn) {
		defer wg.Done()
		defer func() { _ = client.Close() }()
		defer func() { _ = upstream.Close() }()
		buf := make([]byte, 32*1024)
		var local int64
		for {
			n, readErr := src.Read(buf)
			if n > 0 {
				local += int64(n)
				broker.mu.Lock()
				broker.used += int64(n)
				used := broker.used
				broker.mu.Unlock()
				if local > maxTunnelBytes || used > maxProcessBytes {
					_ = dst.Close()
					return
				}
				if writeErr := writeConnAll(dst, buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}
	wg.Add(2)
	go copyOne(upstream, client)
	go copyOne(client, upstream)
	wg.Wait()
}

func writeConnAll(dst net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func resolveExecutable(path, name string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("youtube: %s executable unavailable", name)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("youtube: %s executable unavailable", name)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("youtube: %s executable unavailable", name)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("youtube: %s executable unavailable", name)
	}
	return resolved, nil
}

func systemCABundle() (string, string, error) {
	for _, candidate := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/cert.pem", "/etc/pki/tls/certs/ca-bundle.crt"} {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16<<20 {
			continue
		}
		certDir, err := filepath.EvalSymlinks(filepath.Dir(resolved))
		if err != nil {
			continue
		}
		if dirInfo, err := os.Stat(certDir); err == nil && dirInfo.IsDir() {
			return resolved, certDir, nil
		}
	}
	return "", "", errors.New("youtube: system certificate bundle unavailable")
}

func addPathMount(args []string, path string) []string {
	path = filepath.Clean(path)
	if path == "/usr" || strings.HasPrefix(path, "/usr/") || path == "/bin" || strings.HasPrefix(path, "/bin/") || path == "/lib" || strings.HasPrefix(path, "/lib/") || path == "/lib64" || strings.HasPrefix(path, "/lib64/") {
		return args
	}
	if root, ok := pythonVenvRoot(path); ok {
		args = addParentDirs(args, root)
		return append(args, "--ro-bind", root, root)
	}
	args = addParentDirs(args, path)
	return append(args, "--ro-bind", path, path)
}

// pythonVenvRoot returns the canonical root of a conventional Python virtual
// environment only when the executable is directly in its bin directory and
// the environment contains its own pyvenv.cfg. This avoids mounting arbitrary
// executable parents or following an untrusted script's shebang.
func pythonVenvRoot(executable string) (string, bool) {
	bin, err := filepath.EvalSymlinks(filepath.Dir(executable))
	if err != nil || filepath.Base(bin) != "bin" {
		return "", false
	}
	root, err := filepath.EvalSymlinks(filepath.Dir(bin))
	if err != nil {
		return "", false
	}
	if name := filepath.Base(root); name != ".youtube-venv" && name != "yt-dlp-venv" {
		return "", false
	}
	if filepath.Dir(bin) != root {
		return "", false
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return "", false
	}
	config := filepath.Join(root, "pyvenv.cfg")
	configPath, err := filepath.EvalSymlinks(config)
	if err != nil {
		return "", false
	}
	if !withinPath(root, configPath) {
		return "", false
	}
	configInfo, err := os.Stat(configPath)
	if err != nil || !configInfo.Mode().IsRegular() || configInfo.Size() > 64<<10 {
		return "", false
	}
	canonicalExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil || !withinPath(filepath.Join(root, "bin"), canonicalExecutable) {
		return "", false
	}
	return root, true
}

func withinPath(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func addParentDirs(args []string, path string) []string {
	parent := filepath.Dir(path)
	var dirs []string
	for parent != "/" && parent != "." {
		if parent == "/usr" || parent == "/bin" || parent == "/lib" || parent == "/lib64" {
			break
		}
		dirs = append(dirs, parent)
		parent = filepath.Dir(parent)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		args = append(args, "--dir", dirs[i])
	}
	return args
}

func youtubeHostClass(host string) string {
	switch {
	case host == "youtube.com" || host == "youtu.be" || strings.HasSuffix(host, ".youtube.com"):
		return "youtube"
	case host == "googlevideo.com" || strings.HasSuffix(host, ".googlevideo.com"):
		return "googlevideo"
	default:
		return "other"
	}
}
