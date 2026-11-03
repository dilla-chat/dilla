package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// sessionLedger is the harness's Store plus the one optional method the liveness sweep reads: the
// session rows, keyed by token hash. The harness itself does not have it, so every other test in
// this package runs against a store with no sessions table and a sweep that has nothing to ask.
type sessionLedger struct {
	Store
	mu    sync.Mutex
	rows  map[string]store.SessionRow
	err   error
	reads int
}

func (l *sessionLedger) GetSessionByHash(_ context.Context, tokenHash []byte, now int64) (store.SessionRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	if l.err != nil {
		return store.SessionRow{}, l.err
	}
	row, ok := l.rows[string(tokenHash)]
	if !ok || row.Expires <= now || row.IdleExpires <= now {
		return store.SessionRow{}, store.ErrNotFound
	}
	return row, nil
}

func (l *sessionLedger) put(row store.SessionRow) {
	l.mu.Lock()
	l.rows[string(row.TokenHash)] = row
	l.mu.Unlock()
}

func (l *sessionLedger) drop(tokenHash []byte) {
	l.mu.Lock()
	delete(l.rows, string(tokenHash))
	l.mu.Unlock()
}

func (l *sessionLedger) setErr(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

func (l *sessionLedger) readCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads
}

// sessionHarness is a harness whose gateway reads session rows, with one live connection over one
// session row.
type sessionHarness struct {
	*harness
	ledger *sessionLedger
	conn   *conn
	device id.ID
	hash   []byte
}

func newSessionHarness(t *testing.T) *sessionHarness {
	t.Helper()
	h := newHarness(t)
	ledger := &sessionLedger{Store: h, rows: map[string]store.SessionRow{}}
	h.gw = New(Options{
		Clock: h.clk, Generation: 1, Store: ledger, Auth: h,
	})
	device, user := id.New(), id.New()
	hash := auth.TokenHash("a-session-token-" + device.String())
	now := h.clk.Now().Unix()
	ledger.put(store.SessionRow{
		TokenHash: hash, DeviceID: device, UserID: user,
		Created: now, Expires: now + 30*24*3600, IdleExpires: now + 30*24*3600,
	})
	sink := newRecordingSink(1024, false)
	c, err := h.gw.register(context.Background(),
		auth.Session{DeviceID: device, UserID: user, Scope: auth.ScopeEnrolled, TokenHash: hash}, sink)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	h.mu.Lock()
	h.sinks[c] = sink
	h.mu.Unlock()
	return &sessionHarness{harness: h, ledger: ledger, conn: c, device: device, hash: hash}
}

// dillad admin is a separate process: it deletes the session rows and cannot close a socket. The
// gateway's sweep, one heartbeat interval at most after the rows went, is what ends the connection
// (protocol/02 §2.2 point 6), with the revocation code and no resume window.
func TestASweepClosesAConnectionWhoseSessionRowIsGone(t *testing.T) {
	h := newSessionHarness(t)
	token := h.conn.resumeToken()
	h.ledger.drop(h.hash)

	h.clk.Advance(h.gw.beat.interval + time.Second)
	h.gw.sweepLiveness()

	if code := h.waitClose(t, h.conn); code != CloseSessionRevoked {
		t.Fatalf("close code = %d, want 4004 session_revoked", code)
	}
	if h.gw.Online(h.device) {
		t.Fatal("a device whose session row is gone is still online")
	}
	if _, ok := h.gw.suspended.Load(string(token)); ok {
		t.Fatal("a connection closed 4004 is resumable: its token outlived its session")
	}
}

// The sweep asks once per connection per tick, and a session that still resolves is left alone.
func TestASweepLeavesAConnectionWithALiveSessionAndReadsItOnce(t *testing.T) {
	h := newSessionHarness(t)

	h.clk.Advance(h.gw.beat.interval + time.Second)
	h.gw.sweepLiveness()

	if got := h.ledger.readCount(); got != 1 {
		t.Fatalf("the sweep read the session table %d times for one connection, want 1", got)
	}
	select {
	case code := <-h.sink(h.conn).closed:
		t.Fatalf("a connection with a live session was closed %d", code)
	default:
	}
	if !h.gw.Online(h.device) {
		t.Fatal("a connection with a live session went offline")
	}
}

// A session that has outlived its hard expiry no longer resolves, so the socket it authenticated
// goes with it rather than surviving until a restart.
func TestASweepClosesAConnectionWhoseSessionExpired(t *testing.T) {
	h := newSessionHarness(t)
	h.ledger.put(store.SessionRow{
		TokenHash: h.hash, DeviceID: h.device, UserID: id.New(),
		Created: 1, Expires: h.clk.Now().Unix() + 10, IdleExpires: h.clk.Now().Unix() + 10,
	})

	h.clk.Advance(h.gw.beat.interval)
	h.gw.sweepLiveness()

	if code := h.waitClose(t, h.conn); code != CloseSessionRevoked {
		t.Fatalf("close code = %d, want 4004", code)
	}
}

// A database that cannot answer is not a revocation: the sweep fails open, so a storage hiccup
// does not disconnect every client at once, and the next tick asks again.
func TestASweepThatCannotReadTheSessionTableClosesNothing(t *testing.T) {
	h := newSessionHarness(t)
	h.ledger.setErr(errors.New("database is locked"))

	h.clk.Advance(h.gw.beat.interval)
	h.gw.sweepLiveness()

	select {
	case code := <-h.sink(h.conn).closed:
		t.Fatalf("a storage error closed the connection %d", code)
	default:
	}

	h.ledger.setErr(nil)
	h.ledger.drop(h.hash)
	h.gw.sweepLiveness()
	if code := h.waitClose(t, h.conn); code != CloseSessionRevoked {
		t.Fatalf("close code = %d, want 4004 once the store answers", code)
	}
}

// A store with no session table to read (every other test in this package, and any embedder that
// hands the gateway a narrower Store) makes the check a no-op rather than a refusal.
func TestASweepOverAStoreWithoutSessionsChangesNothing(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())

	h.clk.Advance(h.gw.beat.interval)
	h.gw.sweepLiveness()

	select {
	case code := <-h.sink(c).closed:
		t.Fatalf("closed %d with no session reader configured", code)
	default:
	}
}
