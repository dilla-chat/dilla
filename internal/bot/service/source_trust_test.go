package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type fakeDeviceListSession struct {
	heads   [][]byte
	rows    map[uint64][][]any
	queries []uint64
}

type duplicatePendingTrustNative struct {
	binding     native.Binding
	leaves      []native.GroupPendingLeaf
	calls       int
	revalidated []id.ID
}

func (n *duplicatePendingTrustNative) Binding() native.Binding { return n.binding }
func (n *duplicatePendingTrustNative) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	return append([]native.GroupPendingLeaf(nil), n.leaves...), nil
}
func (*duplicatePendingTrustNative) ProposedAdds(context.Context, id.ID, []byte) ([]native.GroupProposedAdd, error) {
	return nil, nil
}
func (n *duplicatePendingTrustNative) TrustSender(context.Context, id.ID, uint64, id.ID, []byte, []byte, uint64) (native.GroupTrustVerdict, error) {
	n.calls++
	if n.calls > 1 {
		return native.GroupTrustVerdict{}, ErrSenderHistoryStale
	}
	return native.GroupTrustVerdict{}, nil
}
func (*duplicatePendingTrustNative) TrustProposedAdd(context.Context, id.ID, []byte, uint64, id.ID, []byte, []byte, uint64) (native.GroupTrustVerdict, error) {
	return native.GroupTrustVerdict{}, nil
}
func (n *duplicatePendingTrustNative) RevalidateGroup(_ context.Context, group id.ID, _ uint64, _ uint64) (uint32, error) {
	n.revalidated = append(n.revalidated, group)
	return 1, nil
}

func (s *fakeDeviceListSession) OwnDeviceList(context.Context, id.ID) ([]byte, error) {
	if len(s.heads) == 0 {
		return nil, errors.New("no head")
	}
	raw := s.heads[0]
	if len(s.heads) > 1 {
		s.heads = s.heads[1:]
	}
	return raw, nil
}
func (s *fakeDeviceListSession) DeviceListHistory(_ context.Context, _ id.ID, after uint64) ([][]any, error) {
	s.queries = append(s.queries, after)
	return s.rows[after], nil
}

func outerDeviceList(t *testing.T, version uint64, signed []byte) []byte {
	t.Helper()
	raw, err := cborx.Marshal([]any{version, signed, make([]byte, 64), make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReadSenderHistoryPassesInnerSignedListAndCachedAnchor(t *testing.T) {
	user := sid(15)
	row1 := []any{uint64(1), []byte("signed-list-one"), make([]byte, 64), make([]byte, 32)}
	row2 := []any{uint64(2), []byte("signed-list-two"), make([]byte, 64), make([]byte, 32)}
	head2, err := cborx.Marshal(row2)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeDeviceListSession{heads: [][]byte{head2, head2}, rows: map[uint64][][]any{0: {row1, row2}}}
	history, head, version, err := readSenderHistory(context.Background(), s, user, 1)
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 || !reflect.DeepEqual(head, []byte("signed-list-two")) {
		t.Fatalf("inner head=%q version=%d", head, version)
	}
	var got [][]any
	if err = cborx.Unmarshal(history, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0][0] != uint64(1) || got[1][0] != uint64(2) {
		t.Fatalf("history omitted cached anchor: %#v", got)
	}
	if !reflect.DeepEqual(s.queries, []uint64{0}) {
		t.Fatalf("history queried after %v, want cachedVersion-1 anchor", s.queries)
	}
}

func TestReadSenderHistoryRejectsGapBeforeTrust(t *testing.T) {
	user := sid(16)
	head := outerDeviceList(t, 3, []byte("signed-list-three"))
	s := &fakeDeviceListSession{heads: [][]byte{head}, rows: map[uint64][][]any{0: {{uint64(1), []byte("one"), make([]byte, 64), make([]byte, 32)}, {uint64(3), []byte("three"), make([]byte, 64), make([]byte, 32)}}}}
	if _, _, _, err := readSenderHistory(context.Background(), s, user, 1); !errors.Is(err, ErrSenderHistoryStale) {
		t.Fatalf("gap accepted: %v", err)
	}
}

func TestReadSenderHistoryRetriesOnceAfterCurrentHeadMoves(t *testing.T) {
	user := sid(17)
	row1 := []any{uint64(1), []byte("one"), make([]byte, 64), make([]byte, 32)}
	row2 := []any{uint64(2), []byte("two"), make([]byte, 64), make([]byte, 32)}
	row3 := []any{uint64(3), []byte("three"), make([]byte, 64), make([]byte, 32)}
	head2, err := cborx.Marshal(row2)
	if err != nil {
		t.Fatal(err)
	}
	head3, err := cborx.Marshal(row3)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeDeviceListSession{heads: [][]byte{head2, head3, head3, head3}, rows: map[uint64][][]any{0: {row1, row2, row3}}}
	_, head, version, err := readSenderHistory(context.Background(), s, user, 1)
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 || !reflect.DeepEqual(head, []byte("three")) {
		t.Fatalf("after one head race got head=%q version=%d", head, version)
	}
}

func TestReadSenderHistoryRejectsRewindAndVersionBudget(t *testing.T) {
	user := sid(18)
	for _, tc := range []struct {
		name    string
		version uint64
		cached  uint64
	}{
		{name: "rewind", version: 1, cached: 2},
		{name: "version budget", version: 1026, cached: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := outerDeviceList(t, tc.version, []byte("signed-list"))
			s := &fakeDeviceListSession{heads: [][]byte{head}, rows: map[uint64][][]any{}}
			if _, _, _, err := readSenderHistory(context.Background(), s, user, tc.cached); !errors.Is(err, ErrSenderHistoryStale) {
				t.Fatalf("invalid history accepted: %v", err)
			}
		})
	}
}

func TestTrustPhaseErrorsExposeOnlyFixedPhaseAndPreserveCause(t *testing.T) {
	cause := errors.New("private error detail")
	err := trustPhaseError(TrustPhaseSenderTrust, cause)
	var phaseErr *TrustPhaseError
	if !errors.As(err, &phaseErr) || phaseErr.Phase != TrustPhaseSenderTrust {
		t.Fatalf("phase error = %#v", phaseErr)
	}
	if !errors.Is(err, cause) {
		t.Fatal("phase wrapper did not preserve cause")
	}
	if got := err.Error(); got != "sender trust refresh failed" {
		t.Fatalf("phase error leaked cause: %q", got)
	}
	if got := phaseErr.Phase.String(); got != "sender_trust" {
		t.Fatalf("phase label = %q", got)
	}
}

func TestTrustPendingLeavesRefreshesEachUserOncePerBatch(t *testing.T) {
	user := sid(25)
	n := &duplicatePendingTrustNative{
		binding: native.Binding{Instance: sid(26), Community: sid(27), Installation: sid(28)},
		leaves: []native.GroupPendingLeaf{
			{Group: sid(29), Leaf: 1, User: user, CachedVersion: 1},
			{Group: sid(30), Leaf: 2, User: user, CachedVersion: 1},
		},
	}
	trust, err := newSourceTrust(n)
	if err != nil {
		t.Fatal(err)
	}
	head := outerDeviceList(t, 1, []byte("signed-list"))
	session := &fakeDeviceListSession{heads: [][]byte{head, head}}
	recovered, err := trust.trustPendingLeaves(context.Background(), session)
	if err != nil {
		t.Fatalf("same-user stale batch row should be coalesced after first refresh: %v", err)
	}
	if recovered != 2 || n.calls != 1 || !reflect.DeepEqual(n.revalidated, []id.ID{sid(29), sid(30)}) {
		t.Fatalf("recovered=%d trust calls=%d revalidated=%v, want one list refresh and both group revalidations", recovered, n.calls, n.revalidated)
	}
}

func TestTrustPhaseCauseClassIsBounded(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{context.Canceled, "ctx_canceled"}, {context.DeadlineExceeded, "deadline"}, {ErrSenderHistoryStale, "history_stale"}, {ErrSourceTrustBusy, "busy"}, {errors.New("https://private.invalid/secret"), "unknown"}} {
		var e *TrustPhaseError
		if !errors.As(trustPhaseError(TrustPhaseSenderTrust, tc.err), &e) {
			t.Fatal("missing trust phase")
		}
		if got := e.CauseClass(); got != tc.want {
			t.Fatalf("cause class=%q want %q", got, tc.want)
		}
	}
}

type interveningTrustNative struct {
	duplicatePendingTrustNative
	claimed bool
}

func (n *interveningTrustNative) TryClaimTrust(id.ID) (func(), bool) {
	n.claimed = true
	return func() {}, true
}
func (n *interveningTrustNative) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	leaf := n.leaves[0]
	if n.claimed {
		leaf.CachedVersion = 2
	}
	return []native.GroupPendingLeaf{leaf}, nil
}
func (n *interveningTrustNative) TrustSender(_ context.Context, _ id.ID, _ uint64, _ id.ID, history, _ []byte, _ uint64) (native.GroupTrustVerdict, error) {
	var rows [][]any
	if cborx.Unmarshal(history, &rows) != nil || len(rows) != 1 || rows[0][0] != uint64(2) {
		return native.GroupTrustVerdict{}, errors.New("stale cached version")
	}
	n.calls++
	return native.GroupTrustVerdict{}, nil
}
func TestIntakeRereadsCachedAnchorAfterClaim(t *testing.T) {
	user := sid(40)
	n := &interveningTrustNative{duplicatePendingTrustNative: duplicatePendingTrustNative{binding: native.Binding{Instance: sid(41), Community: sid(42), Installation: sid(43)}, leaves: []native.GroupPendingLeaf{{Group: sid(44), User: user, Leaf: 1, CachedVersion: 1}}}}
	trust, err := newSourceTrust(n)
	if err != nil {
		t.Fatal(err)
	}
	head := outerDeviceList(t, 2, []byte("two"))
	session := &fakeDeviceListSession{heads: [][]byte{head, head}, rows: map[uint64][][]any{0: {{uint64(1), []byte("one"), make([]byte, 64), make([]byte, 32)}, {uint64(2), []byte("two"), make([]byte, 64), make([]byte, 32)}}}}
	if _, err = trust.refreshPendingLeaves(t.Context(), session); err != nil || n.calls != 1 {
		t.Fatalf("stale pre-claim intake anchor retained: err=%v trusts=%d", err, n.calls)
	}
}
