package native

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func groupRowForTest(g id.ID, b Binding) []any {
	return []any{g, uint64(0), b.Community, testID(4), uint64(2), uint64(3), uint64(5), uint64(0), uint64(0)}
}
func wrappedGroupBody(t *testing.T, v any) []any {
	t.Helper()
	b, e := cborx.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return []any{b}
}

func TestNativeGroupListAndApplyDecodeStrictly(t *testing.T) {
	b := testBinding()
	g := testID(8)
	body, e := cborx.Marshal([]any{groupRowForTest(g, b)})
	if e != nil {
		t.Fatal(e)
	}
	f := &fakeTransport{responses: [][]any{{body}}}
	c := newFakeClient(t, f)
	got, e := c.Groups(context.Background())
	if e != nil || len(got) != 1 || got[0].ID != g || got[0].State != 2 || got[0].NextSequence != 5 {
		t.Fatalf("groups %#v: %v", got, e)
	}
	bad, _ := cborx.Marshal([]any{[]any{g, uint64(2), b.Community, testID(4), uint64(2), uint64(3), uint64(5), uint64(0), uint64(0)}})
	f = &fakeTransport{responses: [][]any{{bad}}}
	c = newFakeClient(t, f)
	if _, e = c.Groups(context.Background()); e == nil || errors.Is(e, ErrOutcomeUnknown) || f.closed != 0 {
		t.Fatalf("read invalid result semantics: %v closes=%d", e, f.closed)
	}

	applyBody, _ := cborx.Marshal([]any{uint64(2), uint64(9), uint64(10), []any{uint64(8), uint64(9)}, uint64(1), uint64(3)})
	f = &fakeTransport{responses: [][]any{{applyBody}}}
	c = newFakeClient(t, f)
	gotApply, e := c.ApplyGroup(context.Background(), g, []byte{0x80}, []byte{0x80}, 10)
	if e != nil || !gotApply.EpochChanged || !gotApply.OwnAdopted || gotApply.State != 2 || len(gotApply.NewSequences) != 2 || f.groupApplies != 1 || f.calls[0] != protocol.HelperGroupApply {
		t.Fatalf("apply=%#v err=%v staged=%d calls=%v", gotApply, e, f.groupApplies, f.calls)
	}

	malformed, _ := cborx.Marshal([]any{uint64(2), uint64(9), uint64(10), []any{uint64(9), uint64(8)}, uint64(1), uint64(4)})
	f = &fakeTransport{responses: [][]any{{malformed}}}
	c = newFakeClient(t, f)
	if _, e = c.ApplyGroup(context.Background(), g, []byte{0x80}, []byte{0x80}, 10); e == nil || !errors.Is(e, ErrOutcomeUnknown) || f.closed != 1 {
		t.Fatalf("malformed mutation did not quarantine: %v closes=%d", e, f.closed)
	}
}

func TestNativeGroupRequestsValidateLocallyAndUseExactSuboperations(t *testing.T) {
	g := testID(9)
	channel := testID(10)
	op := testID(11)
	f := &fakeTransport{}
	c := newFakeClient(t, f)
	if _, e := c.CreateGroup(context.Background(), id.ID{}, g, channel, 0, 1, make([]byte, 32)); e == nil {
		t.Fatal("zero operation id accepted")
	}
	if _, e := c.CreateGroup(context.Background(), op, g, channel, 2, 1, []byte{1, 0, 0, 0, 0}); e == nil {
		t.Fatal("bad kind/external sender accepted")
	}
	if e := c.GroupRegistered(context.Background(), op, g, 0); e == nil {
		t.Fatal("zero registration sequence accepted")
	}
	if _, e := c.RevalidateGroup(context.Background(), g, 1, 257); e == nil {
		t.Fatal("oversized revalidation page accepted")
	}
	if _, e := c.TrustSender(context.Background(), g, uint64(1)<<32, testID(12), []byte{0x80}, []byte{0x80}, 1); e == nil {
		t.Fatal("out of range leaf accepted")
	}
	if _, e := c.ProposedAdds(context.Background(), g, []byte{0x18, 0x01}); e == nil {
		t.Fatal("noncanonical proposals body accepted")
	}
	if e := c.AcknowledgeGroupEvents(context.Background(), uint64(1)<<63); e == nil {
		t.Fatal("out of range ack accepted")
	}
	if len(f.calls) != 0 || f.groupApplies != 0 {
		t.Fatalf("invalid local requests dispatched: calls=%v staged=%d", f.calls, f.groupApplies)
	}
}

func TestNativeGroupCursorAndIntentBodyRules(t *testing.T) {
	group := testID(14)
	// Cursor body is a nullable cursor body inside the usual body wrapper.
	f := &fakeTransport{responses: [][]any{wrappedGroupBody(t, nil), wrappedGroupBody(t, []any{uint64(4), uint64(7)})}}
	c := newFakeClient(t, f)
	cursor, e := c.GroupCursor(context.Background(), group)
	if e != nil || cursor != nil {
		t.Fatalf("empty cursor %#v %v", cursor, e)
	}
	cursor, e = c.GroupCursor(context.Background(), group)
	if e != nil || cursor == nil || cursor.Sequence != 4 || cursor.Epoch != 7 || len(cursor.Body) == 0 {
		t.Fatalf("cursor %#v %v", cursor, e)
	}

	binding := testBinding()
	bindingBody, _ := cborx.Marshal([]any{uint64(1), binding.Instance, binding.Community, testID(15), uint64(0), uint64(1), uint64(1), uint64(0)})
	createResult, _ := cborx.Marshal([]any{group, bindingBody, []byte{1}, []byte{2}})
	pending, _ := cborx.Marshal([]any{uint64(2), group, uint64(0), createResult})
	resolved, _ := cborx.Marshal([]any{uint64(2), group, uint64(1), nil})
	absent, _ := cborx.Marshal([]any{nil})
	for name, body := range map[string][]byte{"pending": pending, "resolved": resolved, "absent": absent} {
		f = &fakeTransport{responses: [][]any{{body}}}
		c = newFakeClient(t, f)
		got, e := c.GroupIntentResult(context.Background(), testID(13))
		if e != nil {
			t.Fatalf("%s intent: %v", name, e)
		}
		if name == "absent" && got != nil {
			t.Fatalf("absent intent=%#v", got)
		}
		if name != "absent" && got == nil {
			t.Fatalf("%s intent missing", name)
		}
	}
	interactionBinding, _ := cborx.Marshal([]any{uint64(2), testBinding().Instance, nil, testID(16), uint64(3), uint64(1), uint64(1), uint64(0), testBinding().Installation, testBinding().Community, testID(17), testID(18)})
	interactionResult, _ := cborx.Marshal([]any{group, interactionBinding, []byte{1}, []byte{2}, []byte{3}})
	interactionPending, _ := cborx.Marshal([]any{uint64(2), group, uint64(0), interactionResult})
	f = &fakeTransport{responses: [][]any{{interactionPending}}}
	c = newFakeClient(t, f)
	// Bot v2 Task 23: kind-3 groups are gone; a kind-3 create result is no valid intent result.
	if got, e := c.GroupIntentResult(context.Background(), testID(13)); e == nil {
		t.Fatalf("kind-3 pending intent accepted: %#v", got)
	}
	bad, _ := cborx.Marshal([]any{uint64(2), group, uint64(0), nil})
	f = &fakeTransport{responses: [][]any{{bad}}}
	c = newFakeClient(t, f)
	if _, e = c.GroupIntentResult(context.Background(), testID(13)); e == nil {
		t.Fatal("pending intent without exact result was accepted")
	}
}

func TestNativeGroupCommitBuildFiveFieldShapeAndExactBytes(t *testing.T) {
	device := testID(61)
	body, err := cborx.Marshal([]any{uint64(8), []byte{1, 2}, []byte{3, 4}, []any{[]any{device, []byte{5, 6}}}, nil})
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeCommitBuild(body)
	if err != nil {
		t.Fatal(err)
	}
	result := got.(GroupCommitBuildResult)
	if result.Epoch != 8 || !reflect.DeepEqual(result.Commit, []byte{1, 2}) || !reflect.DeepEqual(result.GroupInfo, []byte{3, 4}) || len(result.Welcomes) != 1 || result.Welcomes[0].Device != device || !reflect.DeepEqual(result.Welcomes[0].Welcome, []byte{5, 6}) || !result.ResyncNull || !reflect.DeepEqual(result.ResultBody, body) {
		t.Fatalf("decoded commit result lost core fields or bytes: %#v", result)
	}
	for _, bad := range [][]any{
		{uint64(8), []byte{1}, []byte{2}, []any{}, uint64(0)},
		{uint64(8), []byte{1}, []byte{2}, []any{[]any{device, []byte{3}}, []any{device, []byte{4}}}, nil},
		{uint64(8), []byte{1}, []byte{2}, []any{[]any{device, []byte{3}}}},
	} {
		encoded, _ := cborx.Marshal(bad)
		if _, err := decodeCommitBuild(encoded); err == nil {
			t.Fatalf("accepted malformed commit result %x", encoded)
		}
	}
}

func TestNativeGroupRevalidateReturnsRecoveredCount(t *testing.T) {
	f := &fakeTransport{responses: [][]any{{uint64(513)}}}
	c := newFakeClient(t, f)
	got, err := c.RevalidateGroup(context.Background(), testID(62), 1, 3)
	if err != nil || got != 513 {
		t.Fatalf("revalidation count=%d err=%v", got, err)
	}
}

func TestNativeGroupWelcomesAllowMultipleDeliveriesPerExpectedGroup(t *testing.T) {
	group := testID(63)
	binding := testBinding()
	welcomes := []GroupWelcome{
		{ID: 1, Group: group, Epoch: 4, CommitSequence: 9, Welcome: []byte{1}, RatchetTree: []byte{2}},
		{ID: 2, Group: group, Epoch: 4, CommitSequence: 9, Welcome: []byte{3}, RatchetTree: []byte{2}},
	}
	response, _ := cborx.Marshal([]any{[]any{uint64(1), group, uint64(0), ""}, []any{uint64(2), group, uint64(2), "already joined"}})
	f := &fakeTransport{responses: [][]any{{response}}}
	c := newFakeClient(t, f)
	got, err := c.ApplyWelcomes(context.Background(), welcomes, []GroupExpected{{Group: group, Community: binding.Community, Channel: testID(64)}})
	if err != nil || len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 || got[0].Group != group || got[1].Group != group || got[1].Outcome != 2 {
		t.Fatalf("multiple welcome outcomes %#v err=%v", got, err)
	}
	if _, err := encodeWelcomeRows(append(welcomes, GroupWelcome{ID: 1, Group: group, Welcome: []byte{1}, RatchetTree: []byte{2}})); err == nil {
		t.Fatal("duplicate welcome ID accepted")
	}
}

func TestNativeGroupEventPageValidatesAndRedactsBodies(t *testing.T) {
	b := testBinding()
	g := testID(21)
	body := "private event text"
	row := []any{uint64(2), g, b.Community, testID(22), uint64(4), uint64(5), uint64(6), testID(23), testID(24), uint64(0), uint64(0), uint64(1), testID(25), uint64(0), body, nil, nil}
	page, _ := cborx.Marshal([]any{[]any{row}, uint64(2), uint64(3)})
	f := &fakeTransport{responses: [][]any{{page}}}
	c := newFakeClient(t, f)
	got, e := c.ReadGroupEvents(context.Background(), 1, 10)
	if e != nil || len(got.Events) != 1 || got.Next != 2 || got.Events[0].Body == nil {
		t.Fatalf("events %#v %v", got, e)
	}
	for _, formatted := range []string{fmt.Sprintf("%v", got.Events[0]), fmt.Sprintf("%#v", got.Events[0]), fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got)} {
		if strings.Contains(formatted, body) {
			t.Fatalf("plaintext event leaked: %s", formatted)
		}
	}
	badRow := append([]any(nil), row...)
	badRow[2] = testID(99)
	bad, _ := cborx.Marshal([]any{[]any{badRow}, uint64(2), uint64(3)})
	f = &fakeTransport{responses: [][]any{{bad}}}
	c = newFakeClient(t, f)
	if _, e = c.ReadGroupEvents(context.Background(), 1, 10); e == nil || !errors.Is(e, ErrOutcomeUnknown) || f.closed != 1 {
		t.Fatalf("event read did not quarantine stale-suppression mutation: %v closes=%d", e, f.closed)
	}
}

func TestNativeGroupApplyAllowsBodyAboveDirectFrameLimitAndHonorsCanceledGate(t *testing.T) {
	// One canonical handshake row with a 1.1 MB opaque core blob exercises staged transport
	// without decoding or claiming that the blob is a valid MLS message.
	large := make([]byte, 11+1100000)
	copy(large, []byte{0x81, 0x85, 0x01, 0x00, 0x00, 0xf6, 0x5a, 0x00, 0x10, 0xc8, 0xe0})
	body, _ := cborx.Marshal([]any{uint64(2), uint64(0), uint64(1), []any{}, uint64(0), uint64(0)})
	f := &fakeTransport{responses: [][]any{{body}}}
	c := newFakeClient(t, f)
	if _, e := c.ApplyGroup(context.Background(), testID(31), large, []byte{0x80}, 1); e != nil {
		t.Fatal(e)
	}
	if f.groupApplies != 1 {
		t.Fatalf("large apply bypassed staged path: %d", f.groupApplies)
	}

	f = &fakeTransport{}
	c = newFakeClient(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.ApplyGroup(ctx, testID(31), []byte{0x80}, []byte{0x80}, 1); !errors.Is(e, context.Canceled) {
		t.Fatalf("canceled call: %v", e)
	}
	if len(f.calls) != 0 || f.groupApplies != 0 {
		t.Fatalf("canceled call reached helper: calls=%v staged=%d", f.calls, f.groupApplies)
	}
}

func TestNativeGroupTransportFailureIsNotRetried(t *testing.T) {
	f := &fakeTransport{err: helper.ErrOutcomeUnknown}
	c := newFakeClient(t, f)
	_, e := c.ApplyGroup(context.Background(), testID(41), []byte{0x80}, []byte{0x80}, 1)
	if !errors.Is(e, helper.ErrOutcomeUnknown) || f.groupApplies != 1 || len(f.calls) != 1 {
		t.Fatalf("apply retry/result: %v staged=%d calls=%d", e, f.groupApplies, len(f.calls))
	}
}

func TestGroupExpectedForeignCommunityRejected(t *testing.T) {
	_, e := encodeExpected([]GroupExpected{{Group: testID(50), Community: testID(51), Channel: testID(52)}}, testBinding().Community)
	if e == nil {
		t.Fatal("foreign community accepted in expected group")
	}
}

func TestNativeGroupValidResponseShapeMatrixAllSuboperations(t *testing.T) {
	binding := testBinding()
	g, channel, user := testID(70), testID(71), testID(72)
	bindingBody, _ := cborx.Marshal([]any{uint64(1), binding.Instance, binding.Community, channel, uint64(0), uint64(1), uint64(1), uint64(0)})
	groupRow := groupRowForTest(g, binding)
	event := []any{uint64(1), g, binding.Community, channel, uint64(1), uint64(1), uint64(100), user, testID(73), uint64(0), uint64(0), uint64(0), testID(74), uint64(0), "event", nil, nil}
	commitResult, _ := cborx.Marshal([]any{uint64(0), []byte{1}, []byte{2}, []any{}, nil})
	createResult, _ := cborx.Marshal([]any{g, bindingBody, []byte{1}, []byte{2}})
	checks := []struct {
		op   uint64
		body []byte
		fn   func([]byte) error
	}{
		{0, mustBody(t, []any{groupRow}), func(b []byte) error { _, e := decodeGroupList(b, binding.Community); return e }},
		{1, mustBody(t, groupRow), func(b []byte) error { _, e := decodeOneGroup(b, binding.Community); return e }},
		{2, createResult, func(b []byte) error { _, e := decodeGroupCreate(b, g, binding, nil); return e }},
		{3, nil, func([]byte) error { _, e := voidResult(nil); return e }},
		{4, nil, func([]byte) error { _, e := voidResult(nil); return e }},
		{5, mustBody(t, []any{[]byte{1}, []byte{2}}), func(b []byte) error { _, e := decodeExternalJoin(b); return e }},
		{6, nil, func([]byte) error { _, e := voidResult(nil); return e }},
		{7, mustBody(t, []any{[]any{uint64(1), g, uint64(0), ""}}), func(b []byte) error {
			_, e := decodeWelcomeOutcomes(b, []GroupWelcome{{ID: 1, Group: g}})
			return e
		}},
		{8, mustBody(t, []any{uint64(0), uint64(1), uint64(1), []any{}, uint64(0), uint64(0)}), func(b []byte) error { _, e := decodeApply(b); return e }},
		{9, commitResult, func(b []byte) error { _, e := decodeCommitBuild(b); return e }},
		{10, nil, func([]byte) error { _, e := voidResult(nil); return e }},
		{11, nil, func([]byte) error { _, e := voidResult(nil); return e }},
		{12, mustBody(t, []any{uint64(1), uint64(1)}), func(b []byte) error { _, e := decodeCursor(b); return e }},
		{13, nil, func([]byte) error { _, e := groupUint(uint64(0)); return e }},
		{14, mustBody(t, []any{uint64(1), uint64(1), uint64(0)}), func(b []byte) error { _, e := decodeTrustVerdict(b); return e }},
		{15, mustBody(t, []any{[]any{uint64(0), user, uint64(1)}}), func(b []byte) error {
			v, e := groupBodyArray(b)
			if e != nil {
				return e
			}
			for _, row := range v {
				r := row.([]any)
				if len(r) != 3 {
					return errors.New("proposed-add shape")
				}
				if _, e = groupUint(r[0]); e != nil {
					return e
				}
				if _, e = groupID(r[1]); e != nil {
					return e
				}
				if _, e = groupUint(r[2]); e != nil {
					return e
				}
			}
			return nil
		}},
		{16, mustBody(t, []any{uint64(1), uint64(0), uint64(0)}), func(b []byte) error { _, e := decodeTrustVerdict(b); return e }},
		{17, mustBody(t, []any{[]any{g, uint64(0), user, uint64(1), "unknown"}}), func(b []byte) error {
			v, e := groupBodyArray(b)
			if e != nil {
				return e
			}
			if len(v) != 1 || len(v[0].([]any)) != 5 {
				return errors.New("pending-leaf shape")
			}
			return nil
		}},
		{18, mustBody(t, []any{[]any{event}, uint64(1), uint64(1)}), func(b []byte) error { _, e := decodeGroupEvents(b, binding, 0); return e }},
		{19, nil, func([]byte) error { _, e := voidResult(nil); return e }},
		{20, mustBody(t, []any{nil}), func(b []byte) error {
			v, e := groupBodyArray(b)
			if e != nil {
				return e
			}
			if len(v) != 1 || v[0] != nil {
				return errors.New("intent shape")
			}
			return nil
		}},
	}
	if len(checks) != 21 {
		t.Fatalf("matrix has %d suboperations", len(checks))
	}
	for _, check := range checks {
		if err := check.fn(check.body); err != nil {
			t.Errorf("subop %d valid result rejected: %v", check.op, err)
		}
	}
}

func mustBody(t *testing.T, value any) []byte {
	t.Helper()
	body, err := cborx.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
