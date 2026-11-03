package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/dillad"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// The seeding and inspection half of cliHarness that the admin verbs' tests need. The harness
// itself (init, Run, the held clock) is restore_test.go's.

// Run3 is Run with stdout and stderr kept apart, which is what a test of a usage error or --help
// needs. stderr here is what the verb wrote, then the line exit.Exit would print for the error it
// returned, exactly as Run builds its combined text.
func (h *cliHarness) Run3(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	stdout, stderr, err := run(t, args...)
	h.stderr.WriteString(stderr)
	if err == nil {
		return 0, stdout, stderr
	}
	stderr += "dillad: " + err.Error() + "\n"
	if c, ok := exitCodeOf(err); ok {
		return int(c), stdout, stderr
	}
	return 1, stdout, stderr
}

// exitCodeOf is the exit status an error unwraps to, as exit.Exit reads it.
func exitCodeOf(err error) (exit.Code, bool) {
	var c exit.Code
	if errors.As(err, &c) {
		return c, true
	}
	return 0, false
}

// LogOutput is everything every verb run through this harness wrote to stderr: the structured log
// and the usage and error text.
func (h *cliHarness) LogOutput(t *testing.T) string {
	t.Helper()
	return h.stderr.String()
}

// Repo is one handle on the instance's database that lives as long as the test. Distinct from
// repo, which opens a fresh pair of pools the caller closes.
func (h *cliHarness) Repo(t *testing.T) store.Repository {
	t.Helper()
	if h.shared == nil {
		h.shared = h.repo(t)
		t.Cleanup(func() { _ = h.shared.Close() })
	}
	return h.shared
}

func (h *cliHarness) SeedUser(t *testing.T, username string) id.ID {
	t.Helper()
	u := store.UserRow{
		ID: id.New(), Username: username, Display: strings.ToUpper(username[:1]) + username[1:],
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64),
		Created: h.Now(),
	}
	if err := h.Repo(t).CreateUser(t.Context(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

// SeedFactor gives the user a confirmed TOTP secret, so a dillad.New over the harness's database
// (OpenGatewayConn) does not mark the account as owing a second factor at its boot reconcile
// (web3.6 S1) and the account's sessions reach the gateway.
func (h *cliHarness) SeedFactor(t *testing.T, user id.ID) {
	t.Helper()
	confirmed := h.Now()
	if err := h.Repo(t).PutTOTP(t.Context(), store.TOTPRow{
		UserID: user, Secret: make([]byte, 20), Digits: 6, Period: 30, Algorithm: "SHA1",
		ConfirmedAt: &confirmed, Created: h.Now(),
	}); err != nil {
		t.Fatalf("PutTOTP: %v", err)
	}
}

func (h *cliHarness) SeedCommunity(t *testing.T, owner id.ID) id.ID {
	t.Helper()
	cid := id.New()
	if err := h.Repo(t).CreateCommunity(t.Context(), store.CommunityRow{
		ID: cid, Owner: owner, Name: "Kryptering", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: h.Now(),
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	return cid
}

func (h *cliHarness) SeedDevice(t *testing.T, user id.ID) id.ID {
	t.Helper()
	did := id.New()
	if err := h.Repo(t).CreateDevice(t.Context(), store.DeviceRow{
		ID: did, UserID: user, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		LastSeen: h.Now(), Created: h.Now(),
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return did
}

// SeedSession writes one enrolled session for a fresh device of user and returns its bearer token,
// the way auth.Sessions would have minted it: only SHA-256 of the token is stored.
func (h *cliHarness) SeedSession(t *testing.T, user id.ID) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	token := hex.EncodeToString(raw)
	device := h.SeedDevice(t, user)
	now := h.Now()
	if err := h.Repo(t).CreateSession(t.Context(), store.SessionRow{
		TokenHash: auth.TokenHash(token), DeviceID: device, UserID: user,
		Created: now, Expires: now + 30*24*3600, IdleExpires: now + 30*24*3600,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if h.tokens == nil {
		h.tokens = map[id.ID]string{}
	}
	h.tokens[user] = token
	return token
}

// Invites lists the invites the test made: every one but the bootstrap invite `dillad init` wrote,
// which the config names by its 8-hex reference.
func (h *cliHarness) Invites(t *testing.T) []store.InviteRow {
	t.Helper()
	all, err := h.Repo(t).ListInvites(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListInvites: %v", err)
	}
	var out []store.InviteRow
	for _, inv := range all {
		sum := sha256.Sum256(inv.CodeHash)
		if hex.EncodeToString(sum[:4]) == h.cfg.Registration.AdminInvite {
			continue
		}
		out = append(out, inv)
	}
	return out
}

// RawDB is every byte the database has put on disk: the main file and its write-ahead log, which
// is where a committed row lives until a checkpoint.
func (h *cliHarness) RawDB(t *testing.T) []byte {
	t.Helper()
	var out []byte
	for _, p := range []string{h.cfg.DB.Path, h.cfg.DB.Path + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", p, err)
		}
		out = append(out, b...)
	}
	return out
}

// SetGooseVersion records a migration version no binary knows, the state of a database a newer
// dillad has already migrated.
func (h *cliHarness) SetGooseVersion(t *testing.T, version int64) {
	t.Helper()
	db, err := openSQLiteForTest(h.cfg.DB.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, version); err != nil {
		t.Fatalf("record goose version %d: %v", version, err)
	}
}

// HeartbeatInterval is the instance's gateway.heartbeat_interval.
func (h *cliHarness) HeartbeatInterval() time.Duration {
	return h.cfg.Gateway.HeartbeatInterval.Value()
}

// gatewayConn is a client's end of one gateway connection, read on its own goroutine so the close
// the instance sends is answered the way a live client would.
type gatewayConn struct {
	conn  *websocket.Conn
	ended chan error
}

// OpenGatewayConn serves the instance in this process - the composition root `dillad serve` runs,
// over the same database the verbs write and on the harness's held clock - and brings the user's
// seeded session to `ready` on a real WebSocket.
func (h *cliHarness) OpenGatewayConn(t *testing.T, user id.ID) *gatewayConn {
	t.Helper()
	token, ok := h.tokens[user]
	if !ok {
		t.Fatalf("user %s has no seeded session", user)
	}
	srv, err := dillad.New(t.Context(), dillad.Options{Config: h.cfg, Repo: h.Repo(t), Clock: h.clk})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/gateway", //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("dial /gateway: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	readOp := func() uint64 {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var frame []any
		if err := cborx.Unmarshal(b, &frame); err != nil || len(frame) != 4 {
			t.Fatalf("frame %x: %v", b, err)
		}
		op, _ := frame[0].(uint64)
		return op
	}
	if op := readOp(); op != 0 {
		t.Fatalf("first frame op %d, want hello (0)", op)
	}
	// The wire version the instance offers (web3.8 S1).
	identify, err := cborx.Marshal([]any{uint64(1), uint64(1), nil,
		[]any{"", uint64(dillad.WireVersion), uint64(1), uint64(1), uint64(0)}})
	if err != nil {
		t.Fatalf("marshal identify: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, identify); err != nil {
		t.Fatalf("write identify: %v", err)
	}
	if op := readOp(); op != 3 {
		t.Fatalf("op %d after identify, want ready (3)", op)
	}
	gc := &gatewayConn{conn: c, ended: make(chan error, 1)}
	go func() {
		for {
			if _, _, err := c.Read(t.Context()); err != nil {
				gc.ended <- err
				return
			}
		}
	}()
	return gc
}

// WaitClosed waits for the instance to close the connection and returns the close code it sent.
func (g *gatewayConn) WaitClosed(t *testing.T, d time.Duration) (int, error) {
	t.Helper()
	select {
	case err := <-g.ended:
		return int(websocket.CloseStatus(err)), nil
	case <-time.After(d):
		return 0, context.DeadlineExceeded
	}
}
