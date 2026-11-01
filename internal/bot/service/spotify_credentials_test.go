package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/spotifyauth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type spotifyTestRuntime struct {
	claim client.SpotifyCodeClaim
	err   error
	calls int
}

func (r *spotifyTestRuntime) ClaimSpotifyCode(ctx context.Context, _ id.ID) (client.SpotifyCodeClaim, error) {
	r.calls++
	if err := ctx.Err(); err != nil {
		return client.SpotifyCodeClaim{}, err
	}
	return r.claim, r.err
}

type spotifyTestNative struct {
	mu       sync.Mutex
	binding  native.Binding
	values   map[string]native.StateValue
	fail     bool
	stateErr error
	calls    int
	receipts map[id.ID][]uint64
}

func (n *spotifyTestNative) Binding() native.Binding { return n.binding }
func (n *spotifyTestNative) StateGet(ctx context.Context, area uint64, key []byte) (native.StateValue, error) {
	if err := ctx.Err(); err != nil {
		return native.StateValue{}, err
	}
	if len(key) < 1 || len(key) > 128 {
		return native.StateValue{}, errors.New("invalid native state key")
	}
	if area != spotifyCredentialArea {
		return native.StateValue{}, errors.New("area")
	}
	if n.stateErr != nil {
		return native.StateValue{}, n.stateErr
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.values[string(key)], nil
}
func (n *spotifyTestNative) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if revisions, ok := n.receipts[tx]; ok {
		return append([]uint64(nil), revisions...), nil
	}
	if n.fail {
		return nil, errors.New("transaction")
	}
	c := changes[0]
	if len(c.Key) < 1 || len(c.Key) > 128 {
		return nil, errors.New("invalid native state key")
	}
	v := n.values[string(c.Key)]
	if v.Revision != c.ExpectedRevision {
		return nil, ErrSpotifyConflict
	}
	n.values[string(c.Key)] = native.StateValue{Revision: v.Revision + 1, Value: append([]byte(nil), c.Value...)}
	revisions := []uint64{v.Revision + 1}
	if n.receipts == nil {
		n.receipts = map[id.ID][]uint64{}
	}
	n.receipts[tx] = revisions
	return revisions, nil
}

type spotifyTestExchange struct {
	mu                          sync.Mutex
	exchanges, refreshes        int
	exchangeToken, refreshToken spotifyauth.Token
	err                         error
	seenRefresh                 string
	rotateOnce                  bool
}

type spotifyTestProfile struct {
	account string
	err     error
	calls   int
}

func (p *spotifyTestProfile) AccountID(context.Context, string) (string, error) {
	p.calls++
	return p.account, p.err
}

func (x *spotifyTestExchange) Exchange(ctx context.Context, _, _ string) (spotifyauth.Token, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.exchanges++
	if err := ctx.Err(); err != nil {
		return spotifyauth.Token{}, err
	}
	return x.exchangeToken, x.err
}
func (x *spotifyTestExchange) Refresh(ctx context.Context, refresh string) (spotifyauth.Token, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.refreshes++
	x.seenRefresh = refresh
	if x.rotateOnce && x.refreshes > 1 {
		return spotifyauth.Token{}, errors.New("refresh token already rotated")
	}
	if err := ctx.Err(); err != nil {
		return spotifyauth.Token{}, err
	}
	return x.refreshToken, x.err
}

func spotifyFixture(t *testing.T) (*SpotifyCredentials, *spotifyTestRuntime, *spotifyTestNative, *spotifyTestExchange) {
	b := native.Binding{Instance: testSpotifyID(1), Community: testSpotifyID(2), Installation: testSpotifyID(3)}
	r := &spotifyTestRuntime{claim: client.SpotifyCodeClaim{Attempt: testSpotifyID(4), Code: "one-use", Verifier: "verifier"}}
	n := &spotifyTestNative{binding: b, values: map[string]native.StateValue{}}
	x := &spotifyTestExchange{exchangeToken: spotifyauth.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600}}
	s := newSpotifyCredentials(r, n, x)
	s.profile = &spotifyTestProfile{account: "acct_one"}
	s.guard, _ = NewSpotifyAccountGuard(t.TempDir())
	s.clock = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return s, r, n, x
}
func testSpotifyID(b byte) id.ID { var out id.ID; out[15] = b; return out }

func TestSpotifyCredentialsLinkDuplicateDoesNotExchange(t *testing.T) {
	s, _, n, x := spotifyFixture(t)
	ctx := context.Background()
	if err := s.Link(ctx); err != nil {
		t.Fatal("link failed")
	}
	if err := s.Link(ctx); err != nil {
		t.Fatal("retry failed")
	}
	if x.exchanges != 1 || n.calls != 1 {
		t.Fatalf("exchange/transaction counts %d/%d", x.exchanges, n.calls)
	}
}

func TestSpotifyCredentialsProfileFailurePreventsTokenSave(t *testing.T) {
	s, _, n, _ := spotifyFixture(t)
	s.profile = &spotifyTestProfile{err: errors.New("profile unavailable")}
	if err := s.Link(context.Background()); !errors.Is(err, ErrSpotifyCredentials) {
		t.Fatalf("got %v", err)
	}
	if n.calls != 0 || len(n.values) != 0 {
		t.Fatal("token was saved without a confirmed account ID")
	}
}

func TestSpotifyCredentialsMigratesLegacyStateBeforeLoad(t *testing.T) {
	s, _, n, _ := spotifyFixture(t)
	legacy, err := cborx.Marshal([]any{uint64(1), s.binding.Instance, s.binding.Community, s.binding.Installation, testSpotifyID(7), "legacy-access", "legacy-refresh", uint64(1_800_003_600)})
	if err != nil {
		t.Fatal(err)
	}
	n.values[string(s.key())] = native.StateValue{Revision: 4, Value: legacy}
	tok, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("legacy load failed: %v", err)
	}
	if tok.AccessToken != "legacy-access" || n.values[string(s.key())].Revision != 5 {
		t.Fatal("legacy credentials were not migrated before load")
	}
	state, _, err := s.read(context.Background())
	if err != nil || state.accountID != "acct_one" {
		t.Fatalf("migrated state: %v", err)
	}
}

func TestSpotifyCredentialsMigratesExpiredLegacyStateAfterRefresh(t *testing.T) {
	s, _, n, x := spotifyFixture(t)
	legacy, err := cborx.Marshal([]any{uint64(1), s.binding.Instance, s.binding.Community, s.binding.Installation, testSpotifyID(7), "legacy-access", "legacy-refresh", uint64(1_799_999_000)})
	if err != nil {
		t.Fatal(err)
	}
	n.values[string(s.key())] = native.StateValue{Revision: 2, Value: legacy}
	x.refreshToken = spotifyauth.Token{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresIn: 1800}
	tok, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("legacy refresh and migration failed: %v", err)
	}
	if x.refreshes != 1 || tok.AccessToken != "fresh-access" {
		t.Fatal("expired legacy access token was not refreshed before migration")
	}
}

func TestSpotifyCredentialsFailedMigrationPreservesLegacyState(t *testing.T) {
	s, _, n, _ := spotifyFixture(t)
	legacy, err := cborx.Marshal([]any{uint64(1), s.binding.Instance, s.binding.Community, s.binding.Installation, testSpotifyID(7), "legacy-access", "legacy-refresh", uint64(1_800_003_600)})
	if err != nil {
		t.Fatal(err)
	}
	n.values[string(s.key())] = native.StateValue{Revision: 3, Value: legacy}
	s.profile = &spotifyTestProfile{err: errors.New("profile unavailable")}
	if err := s.ValidateAccount(context.Background()); !errors.Is(err, ErrSpotifyCredentials) {
		t.Fatalf("got %v", err)
	}
	after := n.values[string(s.key())]
	if after.Revision != 3 || string(after.Value) != string(legacy) {
		t.Fatal("failed migration changed or erased legacy credentials")
	}
}

func TestSpotifyCredentialsConcurrentInstancesRotateRefreshTokenOnce(t *testing.T) {
	s, _, n, x := spotifyFixture(t)
	x.rotateOnce = true
	legacy, err := cborx.Marshal([]any{uint64(1), s.binding.Instance, s.binding.Community, s.binding.Installation, testSpotifyID(7), "expired-access", "single-use-refresh", uint64(1_799_999_000)})
	if err != nil {
		t.Fatal(err)
	}
	n.values[string(s.key())] = native.StateValue{Revision: 1, Value: legacy}
	x.refreshToken = spotifyauth.Token{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", ExpiresIn: 3600}
	second := *s
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, credentials := range []*SpotifyCredentials{s, &second} {
		wg.Add(1)
		go func(c *SpotifyCredentials) { defer wg.Done(); _, err := c.Load(context.Background()); errs <- err }(credentials)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Load failed: %v", err)
		}
	}
	if x.refreshes != 1 {
		t.Fatalf("refresh count %d; want 1", x.refreshes)
	}
	state, _, err := s.read(context.Background())
	if err != nil || state.legacy || state.refresh != "rotated-refresh" {
		t.Fatalf("winning rotation was not persisted: %v", err)
	}
}

func TestSpotifyCredentialsChangedAttemptRelinks(t *testing.T) {
	s, r, _, x := spotifyFixture(t)
	if err := s.Link(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.claim.Attempt = testSpotifyID(5)
	x.exchangeToken = spotifyauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600}
	if err := s.Link(context.Background()); err != nil {
		t.Fatal("new attempt should relink")
	}
	if x.exchanges != 2 {
		t.Fatalf("exchange count %d", x.exchanges)
	}
}

func TestSpotifyCredentialsLinkUsesStoredStateWhenBrokerHasNoPendingCode(t *testing.T) {
	s, r, _, x := spotifyFixture(t)
	if err := s.Link(context.Background()); err != nil {
		t.Fatal("initial link failed")
	}
	r.err = client.ErrSpotifyClaimNotFound
	if err := s.Link(context.Background()); err != nil {
		t.Fatalf("stored token should survive expired broker receipt: %v", err)
	}
	if x.exchanges != 1 {
		t.Fatalf("exchange count %d", x.exchanges)
	}
}

func TestSpotifyCredentialsLinkNoPendingAndConsumedCodeRecovery(t *testing.T) {
	t.Run("no pending", func(t *testing.T) {
		s, r, _, _ := spotifyFixture(t)
		r.err = client.ErrSpotifyClaimNotFound
		if err := s.Link(context.Background()); !errors.Is(err, ErrSpotifyNoPending) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("consumed code requires relink", func(t *testing.T) {
		s, _, n, _ := spotifyFixture(t)
		n.fail = true
		if err := s.Link(context.Background()); !errors.Is(err, ErrSpotifyRelinkRequired) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestSpotifyCredentialsRejectWrongBindingMalformedAndTombstone(t *testing.T) {
	t.Run("wrong binding", func(t *testing.T) {
		s, _, _, _ := spotifyFixture(t)
		if err := s.Link(context.Background()); err != nil {
			t.Fatal(err)
		}
		other := *s
		other.binding.Installation = testSpotifyID(9)
		if _, err := other.Load(context.Background()); !errors.Is(err, ErrSpotifyCredentials) {
			t.Fatalf("wrong binding loaded token: %v", err)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		s, _, n, _ := spotifyFixture(t)
		n.values[string(s.key())] = native.StateValue{Revision: 1, Value: []byte{0x80}}
		if _, err := s.Load(context.Background()); !errors.Is(err, ErrSpotifyState) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("tombstone", func(t *testing.T) {
		s, _, n, _ := spotifyFixture(t)
		n.values[string(s.key())] = native.StateValue{Revision: 2, Tombstone: true}
		if _, err := s.Load(context.Background()); !errors.Is(err, ErrSpotifyState) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestSpotifyCredentialsInspectStatusIsReadOnlyAndDistinguishesMissingMalformedAndBinding(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		s, _, n, x := spotifyFixture(t)
		state, reason := s.InspectStatus(context.Background())
		if state != ProviderAccountNotLinked || reason != ProviderReasonMissingCredentials || n.calls != 0 || x.exchanges != 0 || x.refreshes != 0 {
			t.Fatalf("status=%d reason=%d writes=%d exchanges=%d refreshes=%d", state, reason, n.calls, x.exchanges, x.refreshes)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		s, _, n, x := spotifyFixture(t)
		n.values[string(s.key())] = native.StateValue{Revision: 1, Value: []byte{0x80}}
		state, reason := s.InspectStatus(context.Background())
		if state != ProviderAccountNeedsAttention || reason != ProviderReasonInvalidCredentials || n.calls != 0 || x.exchanges != 0 || x.refreshes != 0 {
			t.Fatalf("status=%d reason=%d writes=%d exchanges=%d refreshes=%d", state, reason, n.calls, x.exchanges, x.refreshes)
		}
	})
	t.Run("store unavailable", func(t *testing.T) {
		s, _, n, _ := spotifyFixture(t)
		n.stateErr = errors.New("store")
		state, reason := s.InspectStatus(context.Background())
		if state != ProviderAccountUnknown || reason != ProviderReasonUnavailable || n.calls != 0 {
			t.Fatalf("status=%d reason=%d writes=%d", state, reason, n.calls)
		}
	})
	t.Run("valid stored credential", func(t *testing.T) {
		s, runtime, n, x := spotifyFixture(t)
		if err := s.Link(context.Background()); err != nil {
			t.Fatal(err)
		}
		writes, exchanges, refreshes := n.calls, x.exchanges, x.refreshes
		reader, err := NewSpotifyCredentialStatusReader(runtime, n, s.guard)
		if err != nil {
			t.Fatal(err)
		}
		state, reason := reader.InspectStatus(context.Background())
		if state != ProviderAccountLinked || reason != ProviderReasonNone || n.calls != writes || x.exchanges != exchanges || x.refreshes != refreshes {
			t.Fatalf("status=%d reason=%d writes %d->%d exchanges %d->%d refreshes %d->%d", state, reason, writes, n.calls, exchanges, x.exchanges, refreshes, x.refreshes)
		}
	})
	t.Run("wrong installation payload binding", func(t *testing.T) {
		s, _, n, _ := spotifyFixture(t)
		if err := s.Link(context.Background()); err != nil {
			t.Fatal(err)
		}
		stored := n.values[string(s.key())]
		var fields []any
		if err := cborx.Unmarshal(stored.Value, &fields); err != nil {
			t.Fatal(err)
		}
		wrongBinding := testSpotifyID(9)
		fields[3] = wrongBinding[:]
		stored.Value, _ = cborx.Marshal(fields)
		n.values[string(s.key())] = stored
		state, reason := s.InspectStatus(context.Background())
		if state != ProviderAccountNeedsAttention || reason != ProviderReasonBindingMismatch {
			t.Fatalf("status=%d reason=%d; want binding failure", state, reason)
		}
	})
}

func TestSpotifyCredentialsTransactionFailureAndContext(t *testing.T) {
	s, _, n, x := spotifyFixture(t)
	n.fail = true
	if err := s.Link(context.Background()); !errors.Is(err, ErrSpotifyRelinkRequired) {
		t.Fatalf("got %v", err)
	}
	if x.exchanges != 1 {
		t.Fatalf("exchange count %d", x.exchanges)
	}
	s2, r2, _, x2 := spotifyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s2.Link(ctx); err == nil {
		t.Fatal("canceled context accepted")
	}
	if r2.calls != 0 || x2.exchanges != 0 {
		t.Fatalf("calls %d/%d", r2.calls, x2.exchanges)
	}
}

func TestSpotifyCredentialsStaleIdenticalSaveCannotReplayTransactionReceipt(t *testing.T) {
	s, _, n, _ := spotifyFixture(t)
	state := &spotifyCredentialState{attempt: testSpotifyID(8), access: "access", refresh: "refresh", accountID: "acct_one", expires: 1_800_003_600}
	if err := s.save(context.Background(), state, 0); err != nil {
		t.Fatal("first save", err)
	}
	if err := s.save(context.Background(), state, 0); !errors.Is(err, ErrSpotifyConflict) {
		t.Fatalf("stale identical save returned %v", err)
	}
	if n.calls != 2 {
		t.Fatalf("transaction calls %d", n.calls)
	}
}

func TestSpotifyCredentialsRefreshRotatesRefreshToken(t *testing.T) {
	s, _, n, x := spotifyFixture(t)
	if err := s.Link(context.Background()); err != nil {
		t.Fatal(err)
	}
	x.refreshToken = spotifyauth.Token{AccessToken: "new-access", RefreshToken: "rotated", ExpiresIn: 7200}
	s.clock = func() time.Time { return time.Unix(1_800_004_000, 0) }
	tok, err := s.Load(context.Background())
	if err != nil {
		t.Fatal("refresh failed")
	}
	if tok.AccessToken != "new-access" || x.seenRefresh != "refresh" {
		t.Fatal("unexpected refreshed token")
	}
	persisted, _, err := s.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.refresh != "rotated" || n.values[string(s.key())].Revision != 2 {
		t.Fatal("rotation was not persisted")
	}
}

func TestSpotifyLoadDistinguishesNoAccountFromTransientRead(t *testing.T) {
	credentials, _, state, _ := spotifyFixture(t)
	if _, err := credentials.Load(t.Context()); !errors.Is(err, ErrSpotifyNotLinked) {
		t.Fatalf("missing account=%v", err)
	}
	failure := errors.New("sensitive transient read details")
	state.stateErr = failure
	_, err := credentials.Load(t.Context())
	if errors.Is(err, ErrSpotifyNotLinked) || !errors.Is(err, ErrSpotifyCredentials) || !errors.Is(err, failure) || err.Error() != ErrSpotifyCredentials.Error() {
		t.Fatalf("read failure=%v", err)
	}
}
