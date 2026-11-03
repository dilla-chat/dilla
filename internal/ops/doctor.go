package ops

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pion/turn/v5"

	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Status is how one doctor leg came out.
type Status uint8

// Green is healthy, Yellow is "unknown or degraded, nothing is broken yet" and
// Red is broken. The command exits 69 on any Red and 0 otherwise, so a Yellow
// leg never fails a health check.
const (
	Green Status = iota
	Yellow
	Red
)

// String is the fixed-width-friendly word Report.Text prints.
func (s Status) String() string {
	switch s {
	case Green:
		return "OK"
	case Yellow:
		return "WARN"
	default:
		return "FAIL"
	}
}

// Leg is one check. Fix is the exact command or instruction an operator runs,
// or "" when there is nothing to do.
type Leg struct {
	Name   string
	Status Status
	Detail string
	Fix    string
}

// Report is the whole doctor output, in the order the legs ran.
type Report struct{ Legs []Leg }

// Text is the stable, diffable form: one line per leg, and an indented `fix:`
// line under a leg that has one. The status is padded to 6 columns and the name
// to 13, so keep leg names shorter than 13 characters or the detail abuts them.
// Two runs over the same report are byte-identical, which is what lets a
// runbook diff them.
func (r Report) Text() string {
	var b strings.Builder
	for _, l := range r.Legs {
		fmt.Fprintf(&b, "%-6s%-13s%s\n", l.Status, l.Name, l.Detail)
		if l.Fix != "" {
			fmt.Fprintf(&b, "      fix: %s\n", l.Fix)
		}
	}
	return b.String()
}

// Worst is the most severe status among the legs; an empty report is Green.
func (r Report) Worst() Status {
	var w Status
	for _, l := range r.Legs {
		w = max(w, l.Status)
	}
	return w
}

// Summary is the one line that sums the report up: the worst status, the count
// of checks and how many came out OK, WARN and FAIL. Text does not carry it, so
// a runbook that diffs Text is unaffected.
func (r Report) Summary() string {
	var ok, warn, fail int
	for _, l := range r.Legs {
		switch l.Status {
		case Green:
			ok++
		case Yellow:
			warn++
		default:
			fail++
		}
	}
	return fmt.Sprintf("%-6s%-13s%d checks: %d OK, %d WARN, %d FAIL\n", r.Worst(), "summary", len(r.Legs), ok, warn, fail)
}

// maxClockRTT is the round trip past which a sample is discarded: the midpoint
// estimate's error is at most rtt/2, so a slow answer says little.
const maxClockRTT = 2 * time.Second

// ClockSkew issues a HEAD to url and returns the estimated local-clock offset;
// positive means the local clock is ahead.
//
// HEAD, not GET: there is no body and RFC 9110's MUST still applies. The server
// generated Date somewhere in [t0, t1], so the midpoint is the best single
// estimate, with residual error at most rtt/2 plus Date's one-second
// granularity. Never report a skew below 1s + rtt/2 as non-zero; the spec's 60 s
// threshold has about 60x headroom, which is why it is a sound threshold and a
// tighter one would not be.
func ClockSkew(ctx context.Context, c *http.Client, url string) (time.Duration, error) {
	skew, _, err := clockSample(ctx, c, url)
	return skew, err
}

func clockSample(ctx context.Context, c *http.Client, url string) (skew, rtt time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	t1 := time.Now()
	hdr := resp.Header.Get("Date")
	if hdr == "" {
		return 0, 0, fmt.Errorf("no Date header from %s", url)
	}
	remote, err := http.ParseTime(hdr) // TimeFormat, RFC850 and ANSIC
	if err != nil {
		return 0, 0, fmt.Errorf("unparseable Date %q from %s: %w", hdr, url, err)
	}
	rtt = t1.Sub(t0)
	mid := t0.Add(rtt / 2)
	return mid.Sub(remote), rtt, nil
}

// clockMedian samples every peer, drops the ones that failed or took longer than
// maxClockRTT, and returns the sorted skews with one failure line per dropped
// peer.
func clockMedian(ctx context.Context, c *http.Client, peers []string) (samples []time.Duration, failures []string) {
	for _, p := range peers {
		d, rtt, err := clockSample(ctx, c, p)
		switch {
		case err != nil:
			failures = append(failures, fmt.Sprintf("%s: %v", p, err))
		case rtt > maxClockRTT:
			failures = append(failures, fmt.Sprintf("%s: round trip %s is too slow to judge a clock", p, rtt.Round(time.Millisecond)))
		default:
			samples = append(samples, d)
		}
	}
	slices.Sort(samples)
	return samples, failures
}

// ClockLeg samples every peer, discards samples whose round trip exceeded 2 s,
// and takes the MEDIAN, so one misconfigured origin cannot fail doctor.
func ClockLeg(ctx context.Context, c *http.Client, peers []string, limit time.Duration) Leg {
	samples, failures := clockMedian(ctx, c, peers)
	if len(samples) == 0 {
		return Leg{Name: "clock", Status: Yellow,
			Detail: "no HTTPS origin answered: " + strings.Join(failures, "; "),
			Fix:    "check outbound HTTPS, or set doctor.clock_peers (or pass --clock-peer=https://<a host you can reach>)"}
	}
	med := samples[len(samples)/2]
	if med.Abs() <= limit {
		return Leg{Name: "clock", Status: Green,
			Detail: fmt.Sprintf("within %s of %d HTTPS origins (median %s)", limit, len(samples), med.Round(time.Second))}
	}
	dir := "ahead of"
	if med < 0 {
		dir = "behind"
	}
	return Leg{Name: "clock", Status: Red,
		Detail: fmt.Sprintf("local clock is %s %s %d HTTPS origins (median); ACME order validation and MLS KeyPackage lifetime checks will fail",
			med.Abs().Round(time.Second), dir, len(samples)),
		Fix: "enable systemd-timesyncd or chrony, then re-run dillad doctor"}
}

// WatchClock samples the peers every `every` until ctx ends and hands the median
// skew, in seconds (positive: local clock ahead), to set. It is what keeps the
// dilla_clock_skew_seconds gauge fresh; a round with no usable sample leaves the
// last value alone. serve never gates on it: a booting host with no network yet
// must still start.
func WatchClock(ctx context.Context, c *http.Client, peers []string, every time.Duration, set func(seconds float64)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if samples, _ := clockMedian(ctx, c, peers); len(samples) > 0 {
			set(samples[len(samples)/2].Seconds())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CertificateFromStorage returns a GetCertificate that reads the certificate
// certmagic keeps for domain under storageDir, and never contacts an ACME
// server or starts a renewal: doctor runs beside a live serve and must only
// look. certmagic's FileStorage layout is
// certificates/<issuer>/<domain>/<domain>.crt and .key; when several issuers
// hold one, the certificate that expires last is the one served.
func CertificateFromStorage(storageDir, domain string) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		matches, err := filepath.Glob(filepath.Join(storageDir, "certificates", "*", domain, domain+".crt"))
		if err != nil {
			return nil, err
		}
		var best *tls.Certificate
		for _, crt := range matches {
			key := strings.TrimSuffix(crt, ".crt") + ".key"
			pair, err := tls.LoadX509KeyPair(crt, key)
			if err != nil {
				continue
			}
			leaf, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				continue
			}
			pair.Leaf = leaf
			if best == nil || leaf.NotAfter.After(best.Leaf.NotAfter) {
				c := pair
				best = &c
			}
		}
		if best == nil {
			return nil, fmt.Errorf("no certificate for %s under %s", domain, storageDir)
		}
		return best, nil
	}
}

// renewalWindow is certmagic's default: it starts renewing when a third of the
// certificate's lifetime is left.
const renewalWindow = 3

// CertificateLeg reads the certificate tlsCfg holds for domain and reports its
// issuer and NotAfter. Green outside the renewal window; Yellow inside it, when
// no certificate is held yet, or after a failed renewal (lastRenewalError is ""
// when the most recent issuance or renewal succeeded; certmagic keeps serving the
// old certificate and says nothing, so this is where an operator hears of it);
// Red once the certificate has expired.
func CertificateLeg(_ context.Context, domain string, tlsCfg *tls.Config, lastRenewalError string) Leg {
	const name = "certificate"
	if tlsCfg == nil || tlsCfg.GetCertificate == nil {
		return Leg{Name: name, Status: Yellow, Detail: "no certificate source is configured"}
	}
	cert, err := tlsCfg.GetCertificate(&tls.ClientHelloInfo{ServerName: domain})
	if err != nil || cert == nil {
		detail := "no certificate held for " + domain + " yet"
		if err != nil {
			detail += ": " + err.Error()
		}
		if lastRenewalError != "" {
			detail += "; last issuance failed: " + lastRenewalError
		}
		return Leg{Name: name, Status: Yellow, Detail: detail,
			Fix: "start dillad serve and watch `journalctl -u dilla` for the ACME order; it needs tls.email, an open 443 and a correct clock"}
	}
	leaf := cert.Leaf
	if leaf == nil {
		if len(cert.Certificate) == 0 {
			return Leg{Name: name, Status: Red, Detail: "the certificate for " + domain + " is empty"}
		}
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return Leg{Name: name, Status: Red, Detail: "the certificate for " + domain + " does not parse: " + err.Error()}
		}
	}
	issuer := leaf.Issuer.CommonName
	if len(leaf.Issuer.Organization) > 0 {
		issuer = leaf.Issuer.Organization[0]
	}
	now := time.Now()
	left := leaf.NotAfter.Sub(now)
	switch {
	case left <= 0:
		return Leg{Name: name, Status: Red,
			Detail: fmt.Sprintf("expired %s ago, issuer %s", (-left).Round(time.Minute), issuer),
			Fix:    "check the ACME failure in `journalctl -u dilla`, then `systemctl restart dilla`"}
	case lastRenewalError != "":
		return Leg{Name: name, Status: Yellow,
			Detail: fmt.Sprintf("renewal failed (%s); serving the existing certificate, which expires in %s, issuer %s",
				lastRenewalError, humanDays(left), issuer),
			Fix: "check outbound access to the ACME CA, tls.email and the clock, then `systemctl restart dilla`"}
	case left < leaf.NotAfter.Sub(leaf.NotBefore)/renewalWindow:
		return Leg{Name: name, Status: Yellow,
			Detail: fmt.Sprintf("expires in %s, inside the renewal window, issuer %s", humanDays(left), issuer),
			Fix:    "certmagic renews on its own; if this persists, read the ACME lines of `journalctl -u dilla`"}
	}
	return Leg{Name: name, Status: Green, Detail: fmt.Sprintf("expires in %s, issuer %s", humanDays(left), issuer)}
}

func humanDays(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return d.Round(time.Minute).String()
}

// TURNLeg allocates against the TURN listener at listen with a credential
// minted from secret the way dillad mints one for a device, and reports the
// relayed address. listen is "host:port" for a plain TCP listener (the
// behind_proxy listener) and "tls://host:port" for the one that shares 443
// behind the TLS demux. The probe uses a throwaway device id, so it consumes
// one slot of turn.allocations_per_device for the life of the probe only.
func TURNLeg(ctx context.Context, listen, realm, secret string) Leg {
	const name = "turn"
	relayed, err := turnAllocate(ctx, listen, realm, secret)
	if err != nil && strings.Contains(err.Error(), "error 508") {
		// 508 Insufficient Capacity after a good credential: the relay socket did not bind, which is
		// what a turn.relay_ip that is no address of this host (EADDRNOTAVAIL) looks like from here.
		return Leg{Name: name, Status: Red, Detail: fmt.Sprintf("no allocation on %s: %v", listen, err),
			Fix: "the relay could not bind turn.relay_ip: set it to \"auto\" or to an address of one of this host's " +
				"interfaces (on bridged Docker or a NATed LXC the public IP is not one), then restart dillad"}
	}
	if err != nil {
		return Leg{Name: name, Status: Red, Detail: fmt.Sprintf("no allocation on %s: %v", listen, err),
			Fix: "make sure turn.enabled is true, dillad is running, turn.shared_secret_file matches, and the port is reachable"}
	}
	return Leg{Name: name, Status: Green, Detail: fmt.Sprintf("allocated relay %s on %s", relayed, listen)}
}

func turnAllocate(ctx context.Context, listen, realm, secret string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addr, useTLS := strings.CutPrefix(listen, "tls://")
	var conn net.Conn
	var err error
	if useTLS {
		host, _, herr := net.SplitHostPort(addr)
		if herr != nil {
			return "", herr
		}
		conn, err = (&tls.Dialer{Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return "", err
	}
	stunConn := turn.NewSTUNConn(conn)
	defer func() { _ = stunConn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	// dillad's own credential shape, "<expiry>:<device_id>:<issued>" (the relay refuses pion's
	// two-field one), for a device id no device has: the relay's barred lookup admits an id it has
	// no device row for, which only a holder of the shared secret can mint. Its lifetime is short:
	// the relay refuses a credential that lives longer than turn.credential_ttl, and one issued more
	// than 2 s ahead of its clock (or before it started).
	user, pass := server.TURNCredential(secret, id.New(), 10*time.Second, time.Now())
	client, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: addr, Username: user, Password: pass, Realm: realm, Conn: stunConn,
		RTO: 2 * time.Second,
	})
	if err != nil {
		return "", err
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		return "", err
	}
	relay, err := client.Allocate()
	if err != nil {
		return "", err
	}
	defer func() { _ = relay.Close() }()
	return relay.LocalAddr().String(), nil
}

// UDPLeg contacts nothing. There is no project-run reflector and there will not
// be one, so UDP reachability of the media port can only be verified from
// another network; this leg prints what to publish and what to check.
func UDPLeg(cfg config.Config) Leg {
	const name = "udp"
	if !cfg.LiveKit.Enabled {
		return Leg{Name: name, Status: Green, Detail: "livekit.enabled is false: no media port to reach"}
	}
	if !cfg.Doctor.UDPProbe {
		return Leg{Name: name, Status: Green, Detail: "skipped: doctor.udp_probe is false"}
	}
	ip := "<public_ip>"
	if cfg.Instance.PublicIP.IsValid() {
		ip = cfg.Instance.PublicIP.String()
	}
	return Leg{Name: name, Status: Yellow,
		Detail: fmt.Sprintf("not probed: dillad runs no reflector, so %d/udp can only be checked from outside", cfg.LiveKit.UDPPort),
		Fix: fmt.Sprintf("publish %s:%d/udp to this host and verify it from outside; in behind_proxy that is a raw UDP forward, not an HTTP route",
			ip, cfg.LiveKit.UDPPort)}
}

// WebAuthnLeg reads the passkey configuration and contacts nothing (web3.6 S3). A passkey binds to
// the relying party id forever, and every sign-in needs a second factor now, so a configuration that
// cannot work, or that works only until an operator moves the instance, is worth saying before a
// person is locked out of a new browser:
//   - rp_id that is an IP address fails: browsers refuse a WebAuthn relying party that is not a domain;
//   - an https/http rp_origins host that is neither rp_id nor a subdomain of it fails: the browser
//     refuses the ceremony (custom-scheme origins, like the desktop shell's dilla://app, are not web
//     origins and are not judged);
//   - behind a proxy warns only when no rp_origins entry is the public origin (https, the instance
//     domain, no port or 443): the origin the browser sees is the proxy's;
//   - auth.methods without totp warns: passkeys become the only second factor.
func WebAuthnLeg(cfg config.Config) Leg {
	const name = "webauthn"
	rpID := cfg.Auth.WebAuthn.RPID
	var bad, warn []string
	if _, err := netip.ParseAddr(strings.Trim(rpID, "[]")); err == nil {
		bad = append(bad, fmt.Sprintf("auth.webauthn.rp_id %q is an IP address; browsers accept only a domain name", rpID))
	}
	for _, origin := range cfg.Auth.WebAuthn.RPOrigins {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			continue
		}
		// Hosts are compared without case: a browser lowercases an origin's host, and an
		// operator's https://Chat.Example is the same origin as https://chat.example.
		host, rp := strings.ToLower(u.Hostname()), strings.ToLower(rpID)
		if host != rp && !strings.HasSuffix(host, "."+rp) {
			host = u.Hostname()
			bad = append(bad, fmt.Sprintf("auth.webauthn.rp_origins has %s, whose host %s is neither rp_id %s nor a subdomain of it", origin, host, rpID))
		}
	}
	if cfg.BehindProxy() && !slices.ContainsFunc(cfg.Auth.WebAuthn.RPOrigins, func(origin string) bool {
		u, err := url.Parse(origin)
		return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), cfg.Instance.Domain) &&
			(u.Port() == "" || u.Port() == "443")
	}) {
		warn = append(warn, fmt.Sprintf("tls.mode is behind_proxy and auth.webauthn.rp_origins lacks the public origin https://%s; add the origin the browser shows", cfg.Instance.Domain))
	}
	if !slices.Contains(cfg.Auth.Methods, "totp") {
		warn = append(warn, "auth.methods lacks totp: passkeys are the only second factor; an instance whose passkeys stop working locks new browsers out")
	}
	switch {
	case len(bad) > 0:
		return Leg{Name: name, Status: Red, Detail: strings.Join(append(bad, warn...), "; "),
			Fix: "set auth.webauthn.rp_id to the instance's domain name and every auth.webauthn.rp_origins entry to an origin on it, then restart; passkeys already registered stay bound to the old rp_id"}
	case len(warn) > 0:
		return Leg{Name: name, Status: Yellow, Detail: strings.Join(warn, "; "),
			Fix: "check auth.webauthn and auth.methods in the configuration"}
	}
	return Leg{Name: name, Status: Green, Detail: fmt.Sprintf("rp_id %s, %d %s", rpID, len(cfg.Auth.WebAuthn.RPOrigins), plural(len(cfg.Auth.WebAuthn.RPOrigins), "origin", "origins"))}
}

// blobLegPage is how many blob rows one ListBlobs call returns.
const blobLegPage = 256

// BlobConsistencyLeg walks the blob rows and the blob tree in both directions,
// which a restore can break either way. A referenced row with no file is Red:
// it shows as 404s, and it is recoverable because any client holding the
// plaintext and the key re-uploads the identical bytes (an unreferenced row's
// file is garbage awaiting the sweeper, and its absence is fine). A file with
// no row is Yellow and harmless: a database snapshot older than the blob tree
// leaves those. An upload in flight on a live instance may show as one such
// file for a moment.
func BlobConsistencyLeg(ctx context.Context, repo store.Repository, bs *blob.Store) Leg {
	const name = "blobs"
	var rows, missing int
	var firstMissing []byte
	var after []byte
	for {
		page, err := repo.ListBlobs(ctx, after, blobLegPage)
		if err != nil {
			return Leg{Name: name, Status: Red, Detail: "list blobs: " + err.Error()}
		}
		if len(page) == 0 {
			break
		}
		after = page[len(page)-1].BlobID
		for _, row := range page {
			rows++
			_, err := bs.Stat(row.BlobID)
			switch {
			case errors.Is(err, blob.ErrNotFound):
				if row.UnrefSince == nil {
					if missing == 0 {
						firstMissing = row.BlobID
					}
					missing++
				}
			case err != nil:
				return Leg{Name: name, Status: Red, Detail: fmt.Sprintf("stat %x: %v", row.BlobID, err)}
			}
		}
	}

	var stray int
	var firstStray string
	walkErr := bs.Walk(func(file string, _ int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := hex.DecodeString(file)
		if err == nil && len(raw) == 32 {
			if _, err := repo.GetBlob(ctx, raw); err == nil {
				return nil
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if stray == 0 {
			firstStray = file
		}
		stray++
		return nil
	})
	if walkErr != nil {
		return Leg{Name: name, Status: Red, Detail: "walk the blob directory: " + walkErr.Error()}
	}

	var parts []string
	if missing > 0 {
		parts = append(parts, fmt.Sprintf("%d referenced %s missing (first %s)", missing, plural(missing, "blob", "blobs"), hex.EncodeToString(firstMissing)))
	}
	if stray > 0 {
		parts = append(parts, fmt.Sprintf("%d %s with no row (first %s)", stray, plural(stray, "file", "files"), firstStray))
	}
	switch {
	case missing > 0:
		return Leg{Name: name, Status: Red, Detail: strings.Join(parts, ", "),
			Fix: "restore the blob directory from the same backup as the database; a client that holds the attachment re-uploads identical bytes"}
	case stray > 0:
		return Leg{Name: name, Status: Yellow, Detail: strings.Join(parts, ", ") + "; harmless, left by a database snapshot older than the blob tree"}
	}
	return Leg{Name: name, Status: Green, Detail: fmt.Sprintf("%d %s, every referenced file present, no stray files", rows, plural(rows, "blob", "blobs"))}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ReadSecret reads a secret file the way the TURN server does: the whole file,
// surrounding whitespace removed.
func ReadSecret(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's configured secret file
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s does not exist", path)
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
