package native

import (
	"context"
	"errors"
	"math"
	"unicode/utf8"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	maxGroupBody = 32 << 20
	maxEventBody = 1_047_900
)

type GroupInfo struct {
	ID                                    id.ID
	Kind                                  uint64
	Community                             *id.ID
	Target                                id.ID
	State                                 uint64
	Epoch, NextSequence, ProposalsPending uint64
	PendingCommit                         bool
}

type GroupBinding struct {
	Version                                 uint64
	Instance                                id.ID
	Community                               *id.ID
	Target                                  id.ID
	Kind, Policy, E2EEVersion, MediaVersion uint64
}
type GroupCreateResult struct {
	Group                               id.ID
	Binding                             GroupBinding
	BindingBody, GroupInfo, RatchetTree []byte
}
type GroupExternalJoinResult struct{ Commit, GroupInfo []byte }
type GroupCommitWelcome struct {
	Device  id.ID
	Welcome []byte
}
type GroupCommitBuildResult struct {
	Epoch             uint64
	Commit, GroupInfo []byte
	Welcomes          []GroupCommitWelcome
	ResyncNull        bool
	ResultBody        []byte
}
type GroupApplyResult struct {
	State                    uint64
	Epoch, NextSequence      uint64
	NewSequences             []uint64
	ProposalsPending         uint64
	EpochChanged, OwnAdopted bool
}
type GroupWelcome struct {
	ID                    uint64
	Group                 id.ID
	Epoch, CommitSequence uint64
	Welcome, RatchetTree  []byte
	TreeHash              [32]byte
}
type GroupExpected struct {
	Group, Community, Channel id.ID
	Kind, Policy              uint64
}
type GroupWelcomeOutcome struct {
	ID      uint64
	Group   id.ID
	Outcome uint64
	Reason  string
}
type GroupCursor struct {
	Body            []byte
	Sequence, Epoch uint64
}
type GroupTrustVerdict struct {
	Version     uint64
	Changed     bool
	Quarantined uint64
}
type GroupProposedAdd struct {
	Index         uint32
	User          id.ID
	CachedVersion uint64
}
type GroupPendingLeaf struct {
	Group         id.ID
	Leaf          uint32
	User          id.ID
	CachedVersion uint64
	Reason        string
}
type GroupIntentResult struct {
	Kind   uint64
	Group  id.ID
	State  uint64
	Result []byte
}
type GroupEvent struct {
	DeliveryID                        uint64
	Group, Community, Channel         id.ID
	SourceSequence, Epoch, ReceivedAt uint64
	SenderUser, SenderDevice          id.ID
	SenderKind, SenderTier            uint64
	SenderLeaf                        uint32
	Message                           id.ID
	EnvelopeType                      uint64
	Body                              *string
	ReplyTo, Thread                   *id.ID
}
type GroupEventPage struct {
	Events          []GroupEvent
	Next, HighWater uint64
}

// String redacts plaintext event bodies from ordinary fmt diagnostics.
func (GroupEvent) String() string       { return "GroupEvent{body:[redacted]}" }
func (GroupEvent) GoString() string     { return "native.GroupEvent{body:[redacted]}" }
func (GroupEventPage) String() string   { return "GroupEventPage{events:[redacted]}" }
func (GroupEventPage) GoString() string { return "native.GroupEventPage{events:[redacted]}" }

func groupInputBody(b []byte, maximum int) error {
	if len(b) == 0 || len(b) > maximum || cborx.CheckDeterministic(b) != nil {
		return errors.New("invalid native group body")
	}
	return nil
}
func groupBodyArray(b []byte) ([]any, error) {
	if err := groupInputBody(b, maxGroupBody); err != nil {
		return nil, err
	}
	var a []any
	if err := cborx.Unmarshal(b, &a); err != nil {
		return nil, errors.New("invalid native group body")
	}
	return a, nil
}
func validateInfoAndTree(info, tree []byte) error {
	ia, err := groupBodyArray(info)
	if err != nil || len(ia) != 4 {
		return errors.New("invalid GroupInfo body")
	}
	infoEpoch, err := groupUint(ia[0])
	if err != nil {
		return err
	}
	if _, err = groupBytes(ia[1], 1, maxGroupBody); err != nil {
		return err
	}
	infoHash, ok := ia[2].([]byte)
	if !ok || len(infoHash) != 32 {
		return errors.New("GroupInfo tree hash")
	}
	if _, err = groupUint(ia[3]); err != nil {
		return err
	}
	ta, err := groupBodyArray(tree)
	if err != nil || len(ta) != 3 {
		return errors.New("invalid ratchet tree body")
	}
	treeEpoch, err := groupUint(ta[0])
	if err != nil || treeEpoch != infoEpoch {
		return errors.New("group body epoch mismatch")
	}
	if _, err = groupBytes(ta[1], 1, maxGroupBody); err != nil {
		return err
	}
	treeHash, ok := ta[2].([]byte)
	if !ok || len(treeHash) != 32 || string(treeHash) != string(infoHash) {
		return errors.New("group body tree hash mismatch")
	}
	return nil
}
func validateProposalBody(body []byte) error {
	if err := groupInputBody(body, 4<<20); err != nil {
		return err
	}
	a, err := groupBodyArray(body)
	if err != nil || len(a) > 1024 {
		return errors.New("invalid proposals body")
	}
	for _, x := range a {
		r, ok := x.([]any)
		if !ok || len(r) != 5 {
			return errors.New("proposal row shape")
		}
		if _, err = groupBytes(r[0], 0, 4<<20); err != nil {
			return err
		}
		if _, err = groupUint(r[1]); err != nil {
			return err
		}
		if r[2] != nil {
			n, e := groupUint(r[2])
			if e != nil || n > math.MaxUint32 {
				return errors.New("proposal leaf")
			}
		}
		if _, err = groupBytes(r[3], 0, 4<<20); err != nil {
			return err
		}
		void, e := groupUint(r[4])
		if e != nil || void > 1 {
			return errors.New("proposal void flag")
		}
	}
	return nil
}
func validateHandshakeRows(body []byte) error {
	a, err := groupBodyArray(body)
	if err != nil {
		return err
	}
	seen := make(map[uint64]struct{}, len(a))
	for _, x := range a {
		r, ok := x.([]any)
		if !ok || len(r) != 5 {
			return errors.New("handshake row shape")
		}
		seq, e := groupUint(r[0])
		if e != nil || seq == 0 {
			return errors.New("handshake sequence")
		}
		if _, ok := seen[seq]; ok {
			return errors.New("duplicate handshake sequence")
		}
		seen[seq] = struct{}{}
		if _, e = groupUint(r[1]); e != nil {
			return e
		}
		kind, e := groupUint(r[2])
		if e != nil || kind > 2 {
			return errors.New("handshake kind")
		}
		if r[3] != nil {
			leaf, e := groupUint(r[3])
			if e != nil || leaf > math.MaxUint32 {
				return errors.New("handshake leaf")
			}
		}
		if _, e = groupBytes(r[4], 0, maxGroupBody); e != nil {
			return e
		}
	}
	return nil
}
func validateMessageRows(body []byte) error {
	a, err := groupBodyArray(body)
	if err != nil {
		return err
	}
	seen := make(map[uint64]struct{}, len(a))
	for _, x := range a {
		r, ok := x.([]any)
		if !ok || (len(r) != 8 && len(r) != 9) {
			return errors.New("message row shape")
		}
		seq, e := groupUint(r[0])
		if e != nil || seq == 0 {
			return errors.New("message sequence")
		}
		if _, ok := seen[seq]; ok {
			return errors.New("duplicate message sequence")
		}
		seen[seq] = struct{}{}
		if _, e = groupUint(r[1]); e != nil {
			return e
		}
		if _, e = groupID(r[2]); e != nil {
			return e
		}
		if r[3] != nil {
			if _, e = groupBytes(r[3], 0, maxGroupBody); e != nil {
				return e
			}
		}
		if r[4] != nil {
			if _, e = groupBytes(r[4], 32, 32); e != nil {
				return e
			}
		}
		if _, e = groupBytes(r[5], 32, 32); e != nil {
			return e
		}
		if _, e = groupUint(r[6]); e != nil {
			return e
		}
		deleted, e := groupUint(r[7])
		if e != nil || deleted > 1 {
			return errors.New("message deleted flag")
		}
		if len(r) == 9 && r[8] != nil {
			class, e := groupUint(r[8])
			if e != nil || class > 1 {
				return errors.New("message class")
			}
		}
	}
	return nil
}
func groupID(v any) (id.ID, error) {
	b, ok := v.([]byte)
	if !ok || len(b) != 16 {
		return id.ID{}, errors.New("identifier shape")
	}
	var out id.ID
	copy(out[:], b)
	if out.IsZero() {
		return id.ID{}, errors.New("zero identifier")
	}
	return out, nil
}
func nullableGroupID(v any) (*id.ID, error) {
	if v == nil {
		return nil, nil
	}
	x, e := groupID(v)
	if e != nil {
		return nil, e
	}
	return &x, nil
}
func groupUint(v any) (uint64, error) {
	n, ok := v.(uint64)
	if !ok || n > math.MaxInt64 {
		return 0, errors.New("integer shape")
	}
	return n, nil
}
func groupBytes(v any, minimum, maximum int) ([]byte, error) {
	b, ok := v.([]byte)
	if !ok || len(b) < minimum || len(b) > maximum {
		return nil, errors.New("byte string shape")
	}
	return append([]byte(nil), b...), nil
}
func groupText(v any, maximum int) (string, error) {
	s, ok := v.(string)
	if !ok || !utf8.ValidString(s) || len([]byte(s)) > maximum {
		return "", errors.New("text shape")
	}
	return s, nil
}
func groupWrapped(fn func([]byte) (any, error)) func([]any) (any, error) {
	return func(a []any) (any, error) {
		if len(a) != 1 {
			return nil, errors.New("wrapped group result shape")
		}
		b, e := groupBytes(a[0], 1, maxGroupBody)
		if e != nil {
			return nil, e
		}
		return fn(b)
	}
}
func groupDecodeRow(a []any, community id.ID) (GroupInfo, error) {
	var out GroupInfo
	if len(a) != 9 {
		return out, errors.New("group row shape")
	}
	var e error
	if out.ID, e = groupID(a[0]); e != nil {
		return out, e
	}
	if out.Kind, e = groupUint(a[1]); e != nil || out.Kind > 1 {
		return out, errors.New("group kind")
	}
	if out.Community, e = nullableGroupID(a[2]); e != nil {
		return out, e
	}
	if out.Community == nil || *out.Community != community {
		return out, errors.New("group community binding")
	}
	if out.Target, e = groupID(a[3]); e != nil {
		return out, e
	}
	if out.State, e = groupUint(a[4]); e != nil || out.State > 4 {
		return out, errors.New("group state")
	}
	if out.Epoch, e = groupUint(a[5]); e != nil {
		return out, e
	}
	if out.NextSequence, e = groupUint(a[6]); e != nil || out.NextSequence == 0 {
		return out, errors.New("group sequence")
	}
	if out.ProposalsPending, e = groupUint(a[7]); e != nil {
		return out, e
	}
	pending, e := groupUint(a[8])
	if e != nil || pending > 1 {
		return out, errors.New("pending commit flag")
	}
	out.PendingCommit = pending == 1
	return out, nil
}
func decodeGroupList(b []byte, community id.ID) (any, error) {
	a, e := groupBodyArray(b)
	if e != nil {
		return nil, e
	}
	out := make([]GroupInfo, 0, len(a))
	for _, v := range a {
		row, ok := v.([]any)
		if !ok {
			return nil, errors.New("group list row")
		}
		g, e := groupDecodeRow(row, community)
		if e != nil {
			return nil, e
		}
		if len(out) > 0 && string(out[len(out)-1].ID[:]) >= string(g.ID[:]) {
			return nil, errors.New("group list order")
		}
		out = append(out, g)
	}
	return out, nil
}
func decodeOneGroup(b []byte, community id.ID) (any, error) {
	a, e := groupBodyArray(b)
	if e != nil {
		return nil, e
	}
	return groupDecodeRow(a, community)
}
func checkedGroupID(x id.ID) error {
	if x.IsZero() {
		return errors.New("zero identifier")
	}
	return nil
}
func (c *Client) groupCall(ctx context.Context, payload []any, mutation bool, decode func([]any) (any, error)) (any, error) {
	return c.call(ctx, protocol.HelperGroupApply, payload, mutation, decode)
}
func groupBodyPayload(b []byte) any { return append([]byte(nil), b...) }

func (c *Client) Groups(ctx context.Context) ([]GroupInfo, error) {
	v, e := c.groupCall(ctx, []any{uint64(0)}, false, groupWrapped(func(b []byte) (any, error) { return decodeGroupList(b, c.binding.Community) }))
	if e != nil {
		return nil, e
	}
	return resultAs[[]GroupInfo](v)
}

func (c *Client) InspectGroup(ctx context.Context, group id.ID) (GroupInfo, error) {
	if e := checkedGroupID(group); e != nil {
		return GroupInfo{}, e
	}
	v, e := c.groupCall(ctx, []any{uint64(1), group}, false, groupWrapped(func(b []byte) (any, error) { return decodeOneGroup(b, c.binding.Community) }))
	if e != nil {
		return GroupInfo{}, e
	}
	return resultAs[GroupInfo](v)
}

func decodeGroupCreate(b []byte, want id.ID, binding Binding, expected *GroupExpected) (any, error) {
	a, e := groupBodyArray(b)
	if e != nil || len(a) != 4 {
		return nil, errors.New("group create result shape")
	}
	g, e := groupID(a[0])
	if e != nil || g != want {
		return nil, errors.New("group create identity")
	}
	bind, e := groupBytes(a[1], 1, maxGroupBody)
	if e != nil || cborx.ExpectMajor(bind, cborx.MajorArray) != nil {
		return nil, errors.New("group binding body")
	}
	ba, e := groupBodyArray(bind)
	if e != nil || len(ba) != 8 {
		return nil, errors.New("group binding body")
	}
	var gb GroupBinding
	if gb.Version, e = groupUint(ba[0]); e != nil || gb.Version != 1 {
		return nil, errors.New("group binding version")
	}
	if gb.Instance, e = groupID(ba[1]); e != nil || gb.Instance != binding.Instance {
		return nil, errors.New("group binding instance")
	}
	if gb.Community, e = nullableGroupID(ba[2]); e != nil || gb.Community == nil || *gb.Community != binding.Community {
		return nil, errors.New("group binding community")
	}
	if gb.Target, e = groupID(ba[3]); e != nil {
		return nil, errors.New("group binding target")
	}
	if gb.Kind, e = groupUint(ba[4]); e != nil || gb.Kind > 1 {
		return nil, errors.New("group binding kind")
	}
	if gb.Policy, e = groupUint(ba[5]); e != nil {
		return nil, errors.New("group binding policy")
	}
	if gb.E2EEVersion, e = groupUint(ba[6]); e != nil || gb.E2EEVersion != 1 {
		return nil, errors.New("group binding E2EE version")
	}
	if gb.MediaVersion, e = groupUint(ba[7]); e != nil || gb.MediaVersion > 1 {
		return nil, errors.New("group binding media version")
	}
	if (gb.Kind == 0 && gb.MediaVersion != 0) || (gb.Kind == 1 && gb.MediaVersion != 1) {
		return nil, errors.New("group binding media kind")
	}
	if expected != nil && (gb.Target != expected.Channel || gb.Kind != expected.Kind || gb.Policy != expected.Policy) {
		return nil, errors.New("group binding request context")
	}
	gi, e := groupBytes(a[2], 1, maxGroupBody)
	if e != nil {
		return nil, e
	}
	tree, e := groupBytes(a[3], 1, maxGroupBody)
	if e != nil {
		return nil, e
	}
	return GroupCreateResult{Group: g, Binding: gb, BindingBody: bind, GroupInfo: gi, RatchetTree: tree}, nil
}
func (c *Client) CreateGroup(ctx context.Context, operation, group, channel id.ID, kind, policy uint64, externalSender []byte) (GroupCreateResult, error) {
	if operation.IsZero() || group.IsZero() || channel.IsZero() || kind > 1 || policy > math.MaxInt64 || len(externalSender) != 32 || zero(externalSender) {
		return GroupCreateResult{}, errors.New("invalid group create input")
	}
	expected := &GroupExpected{Group: group, Community: c.binding.Community, Channel: channel, Kind: kind, Policy: policy}
	v, e := c.groupCall(ctx, []any{uint64(2), operation, group, channel, kind, policy, append([]byte(nil), externalSender...)}, true, groupWrapped(func(b []byte) (any, error) { return decodeGroupCreate(b, group, c.binding, expected) }))
	if e != nil {
		return GroupCreateResult{}, e
	}
	return resultAs[GroupCreateResult](v)
}

func (c *Client) GroupRegistered(ctx context.Context, operation, group id.ID, next uint64) error {
	if operation.IsZero() || group.IsZero() || next == 0 || next > math.MaxInt64 {
		return errors.New("invalid group registration input")
	}
	_, e := c.groupCall(ctx, []any{uint64(3), operation, group, next}, true, voidResult)
	return e
}
func (c *Client) DiscardGroup(ctx context.Context, operation, group id.ID) error {
	if operation.IsZero() || group.IsZero() {
		return errors.New("invalid group discard input")
	}
	_, e := c.groupCall(ctx, []any{uint64(4), operation, group}, true, voidResult)
	return e
}
func decodeExternalJoin(b []byte) (any, error) {
	a, e := groupBodyArray(b)
	if e != nil || len(a) != 2 {
		return nil, errors.New("external join result shape")
	}
	commit, e := groupBytes(a[0], 1, maxGroupBody)
	if e != nil {
		return nil, e
	}
	gi, e := groupBytes(a[1], 1, maxGroupBody)
	if e != nil {
		return nil, e
	}
	return GroupExternalJoinResult{Commit: commit, GroupInfo: gi}, nil
}
func (c *Client) ExternalJoin(ctx context.Context, operation, group, channel id.ID, kind, policy uint64, groupInfo, tree []byte) (GroupExternalJoinResult, error) {
	if operation.IsZero() || group.IsZero() || channel.IsZero() || kind > 1 || policy > math.MaxInt64 || validateInfoAndTree(groupInfo, tree) != nil {
		return GroupExternalJoinResult{}, errors.New("invalid external join input")
	}
	v, e := c.groupCall(ctx, []any{uint64(5), operation, group, channel, kind, policy, groupBodyPayload(groupInfo), groupBodyPayload(tree)}, true, groupWrapped(decodeExternalJoin))
	if e != nil {
		return GroupExternalJoinResult{}, e
	}
	return resultAs[GroupExternalJoinResult](v)
}
func (c *Client) GroupJoined(ctx context.Context, operation, group id.ID, sequence uint64) error {
	if operation.IsZero() || group.IsZero() || sequence == 0 || sequence > math.MaxInt64 {
		return errors.New("invalid group joined input")
	}
	_, e := c.groupCall(ctx, []any{uint64(6), operation, group, sequence}, true, voidResult)
	return e
}

func encodeWelcomeRows(rows []GroupWelcome) ([]byte, error) {
	values := make([]any, 0, len(rows))
	seenIDs := map[uint64]bool{}
	for _, w := range rows {
		if w.ID == 0 || w.ID > math.MaxInt64 || w.Group.IsZero() || seenIDs[w.ID] || w.Epoch > math.MaxInt64 || w.CommitSequence > math.MaxInt64 || len(w.Welcome) == 0 || len(w.Welcome) > maxGroupBody || len(w.RatchetTree) == 0 || len(w.RatchetTree) > maxGroupBody {
			return nil, errors.New("invalid welcome")
		}
		seenIDs[w.ID] = true
		values = append(values, []any{w.ID, w.Group, w.Epoch, w.CommitSequence, groupBodyPayload(w.Welcome), groupBodyPayload(w.RatchetTree), w.TreeHash[:]})
	}
	b, err := cborx.Marshal(values)
	if err != nil || len(b) > maxGroupBody {
		return nil, errors.New("welcome input exceeds staged limit")
	}
	return b, nil
}
func encodeExpected(rows []GroupExpected, community id.ID) ([]byte, error) {
	values := make([]any, 0, len(rows))
	seen := map[id.ID]bool{}
	for _, g := range rows {
		if g.Group.IsZero() || g.Channel.IsZero() || g.Community.IsZero() || g.Community != community || g.Kind > 1 || g.Policy > math.MaxInt64 || seen[g.Group] {
			return nil, errors.New("invalid expected group")
		}
		seen[g.Group] = true
		values = append(values, []any{g.Group, g.Community, g.Channel, g.Kind, g.Policy})
	}
	b, err := cborx.Marshal(values)
	if err != nil || len(b) > maxGroupBody {
		return nil, errors.New("expected group input exceeds staged limit")
	}
	return b, nil
}
func decodeWelcomeOutcomes(b []byte, welcomes []GroupWelcome) (any, error) {
	a, e := groupBodyArray(b)
	if e != nil || len(a) != len(welcomes) {
		return nil, errors.New("welcome outcome count")
	}
	byID := map[uint64]GroupWelcome{}
	for _, w := range welcomes {
		byID[w.ID] = w
	}
	out := make([]GroupWelcomeOutcome, 0, len(a))
	seen := map[uint64]bool{}
	for i, welcome := range welcomes {
		v := a[i]
		r, ok := v.([]any)
		if !ok || len(r) != 4 {
			return nil, errors.New("welcome outcome shape")
		}
		idn, e := groupUint(r[0])
		if e != nil || idn != welcome.ID {
			return nil, errors.New("welcome order")
		}
		group, e := groupID(r[1])
		if e != nil || group != welcome.Group || seen[idn] {
			return nil, errors.New("welcome group")
		}
		seen[idn] = true
		status, e := groupUint(r[2])
		if e != nil || status > 3 {
			return nil, errors.New("welcome status")
		}
		reason, e := groupText(r[3], 1024)
		if e != nil {
			return nil, e
		}
		if w, ok := byID[idn]; !ok || w.Group != group {
			return nil, errors.New("unexpected welcome group")
		}
		out = append(out, GroupWelcomeOutcome{ID: idn, Group: group, Outcome: status, Reason: reason})
	}
	return out, nil
}
func (c *Client) ApplyWelcomes(ctx context.Context, welcomes []GroupWelcome, expected []GroupExpected) ([]GroupWelcomeOutcome, error) {
	wb, e := encodeWelcomeRows(welcomes)
	if e != nil {
		return nil, e
	}
	eb, e := encodeExpected(expected, c.binding.Community)
	if e != nil {
		return nil, e
	}
	welcomeGroups := make(map[id.ID]struct{}, len(welcomes))
	for _, w := range welcomes {
		welcomeGroups[w.Group] = struct{}{}
	}
	for _, g := range expected {
		if _, ok := welcomeGroups[g.Group]; !ok {
			return nil, errors.New("welcome/expected group mismatch")
		}
	}
	if len(welcomeGroups) != len(expected) {
		return nil, errors.New("welcome/expected group mismatch")
	}
	if len(wb)+len(eb)+32 > maxGroupBody {
		return nil, errors.New("welcome operation exceeds staged limit")
	}
	v, e := c.groupCall(ctx, []any{uint64(7), wb, eb}, true, groupWrapped(func(b []byte) (any, error) { return decodeWelcomeOutcomes(b, welcomes) }))
	if e != nil {
		return nil, e
	}
	return resultAs[[]GroupWelcomeOutcome](v)
}

func decodeApply(b []byte) (any, error) {
	a, e := groupBodyArray(b)
	if e != nil || len(a) != 6 {
		return nil, errors.New("apply result shape")
	}
	var out GroupApplyResult
	if out.State, e = groupUint(a[0]); e != nil || out.State > 4 {
		return nil, errors.New("apply state")
	}
	if out.Epoch, e = groupUint(a[1]); e != nil {
		return nil, e
	}
	if out.NextSequence, e = groupUint(a[2]); e != nil || out.NextSequence == 0 {
		return nil, errors.New("apply sequence")
	}
	rows, ok := a[3].([]any)
	if !ok || len(rows) > maxGroupBody/2 {
		return nil, errors.New("apply sequences")
	}
	out.NewSequences = make([]uint64, 0, len(rows))
	var prev uint64
	for _, x := range rows {
		n, e := groupUint(x)
		if e != nil || n == 0 || n <= prev {
			return nil, errors.New("apply sequence order")
		}
		out.NewSequences = append(out.NewSequences, n)
		prev = n
	}
	if out.ProposalsPending, e = groupUint(a[4]); e != nil {
		return nil, e
	}
	flags, e := groupUint(a[5])
	if e != nil || flags&^uint64(3) != 0 {
		return nil, errors.New("apply flags")
	}
	out.EpochChanged = flags&1 != 0
	out.OwnAdopted = flags&2 != 0
	return out, nil
}
func (c *Client) ApplyGroup(ctx context.Context, group id.ID, handshakes, messages []byte, through uint64) (GroupApplyResult, error) {
	if group.IsZero() || validateHandshakeRows(handshakes) != nil || validateMessageRows(messages) != nil || through >= math.MaxInt64 {
		return GroupApplyResult{}, errors.New("invalid group apply input")
	}
	v, e := c.groupCall(ctx, []any{uint64(8), group, groupBodyPayload(handshakes), groupBodyPayload(messages), through}, true, groupWrapped(decodeApply))
	if e != nil {
		return GroupApplyResult{}, e
	}
	return resultAs[GroupApplyResult](v)
}

func decodeCommitBuild(b []byte) (any, error) {
	a, e := groupBodyArray(b)
	if e != nil || len(a) != 5 {
		return nil, errors.New("commit build result shape")
	}
	epoch, e := groupUint(a[0])
	if e != nil {
		return nil, e
	}
	commit, e := groupBytes(a[1], 1, maxGroupBody)
	if e != nil {
		return nil, e
	}
	info, e := groupBytes(a[2], 1, maxGroupBody)
	if e != nil {
		return nil, e
	}
	rows, ok := a[3].([]any)
	if !ok || len(rows) > maxGroupBody/2 {
		return nil, errors.New("commit welcome rows")
	}
	welcomes := make([]GroupCommitWelcome, 0, len(rows))
	seen := map[id.ID]bool{}
	for _, row := range rows {
		pair, ok := row.([]any)
		if !ok || len(pair) != 2 {
			return nil, errors.New("commit welcome shape")
		}
		device, e := groupID(pair[0])
		if e != nil || seen[device] {
			return nil, errors.New("commit welcome device")
		}
		welcome, e := groupBytes(pair[1], 1, maxGroupBody)
		if e != nil {
			return nil, e
		}
		seen[device] = true
		welcomes = append(welcomes, GroupCommitWelcome{Device: device, Welcome: welcome})
	}
	if a[4] != nil {
		return nil, errors.New("commit resync marker")
	}
	return GroupCommitBuildResult{Epoch: epoch, Commit: commit, GroupInfo: info, Welcomes: welcomes, ResyncNull: true, ResultBody: append([]byte(nil), b...)}, nil
}
func (c *Client) BuildCommit(ctx context.Context, operation, group id.ID, proposals []byte) (GroupCommitBuildResult, error) {
	if operation.IsZero() || group.IsZero() || validateProposalBody(proposals) != nil {
		return GroupCommitBuildResult{}, errors.New("invalid commit build input")
	}
	v, e := c.groupCall(ctx, []any{uint64(9), operation, group, groupBodyPayload(proposals)}, true, groupWrapped(decodeCommitBuild))
	if e != nil {
		return GroupCommitBuildResult{}, e
	}
	return resultAs[GroupCommitBuildResult](v)
}
func (c *Client) ConfirmCommit(ctx context.Context, operation, group id.ID) error {
	if operation.IsZero() || group.IsZero() {
		return errors.New("invalid commit confirmation")
	}
	_, e := c.groupCall(ctx, []any{uint64(10), operation, group}, true, voidResult)
	return e
}
func (c *Client) AbortCommit(ctx context.Context, operation, group id.ID) error {
	if operation.IsZero() || group.IsZero() {
		return errors.New("invalid commit abort")
	}
	_, e := c.groupCall(ctx, []any{uint64(11), operation, group}, true, voidResult)
	return e
}

func decodeCursor(b []byte) (any, error) {
	if len(b) == 1 && b[0] == 0xf6 {
		return (*GroupCursor)(nil), nil
	}
	a, e := groupBodyArray(b)
	if e != nil || len(a) != 2 {
		return nil, errors.New("cursor shape")
	}
	seq, e := groupUint(a[0])
	if e != nil || seq == 0 {
		return nil, errors.New("cursor sequence")
	}
	epoch, e := groupUint(a[1])
	if e != nil {
		return nil, e
	}
	return &GroupCursor{Body: append([]byte(nil), b...), Sequence: seq, Epoch: epoch}, nil
}
func (c *Client) GroupCursor(ctx context.Context, group id.ID) (*GroupCursor, error) {
	if group.IsZero() {
		return nil, errors.New("invalid group cursor input")
	}
	v, e := c.groupCall(ctx, []any{uint64(12), group}, false, groupWrapped(decodeCursor))
	if e != nil {
		return nil, e
	}
	return resultAs[*GroupCursor](v)
}
func (c *Client) AcknowledgeGroupCursor(ctx context.Context, group id.ID, cursor GroupCursor) error {
	if group.IsZero() || cursor.Sequence == 0 || cursor.Sequence > math.MaxInt64 || cursor.Epoch > math.MaxInt64 {
		return errors.New("invalid cursor acknowledgement")
	}
	if err := groupInputBody(cursor.Body, 128); err != nil {
		return errors.New("invalid cursor body")
	}
	a, e := groupBodyArray(cursor.Body)
	if e != nil || len(a) != 2 || a[0] != cursor.Sequence || a[1] != cursor.Epoch {
		return errors.New("cursor body mismatch")
	}
	_, e = c.call(ctx, protocol.HelperCursorAck, []any{group, cursor.Sequence, cursor.Epoch}, true, voidResult)
	return e
}
func (c *Client) RevalidateGroup(ctx context.Context, group id.ID, leaf uint64, limit uint64) (uint32, error) {
	if group.IsZero() || leaf > math.MaxUint32 || limit < 1 || limit > 256 {
		return 0, errors.New("invalid revalidation input")
	}
	v, e := c.groupCall(ctx, []any{uint64(13), group, leaf, limit}, true, func(a []any) (any, error) {
		if len(a) != 1 {
			return nil, errors.New("revalidate shape")
		}
		n, e := groupUint(a[0])
		if e != nil || n > math.MaxUint32 {
			return nil, errors.New("revalidate value")
		}
		return uint32(n), nil
	})
	if e != nil {
		return 0, e
	}
	return resultAs[uint32](v)
}
func decodeTrustVerdict(b []byte) (any, error) {
	a, e := groupBodyArray(b)
	if e != nil || len(a) != 3 {
		return nil, errors.New("trust verdict shape")
	}
	version, e := groupUint(a[0])
	if e != nil {
		return nil, e
	}
	changed, e := groupUint(a[1])
	if e != nil || changed > 1 {
		return nil, errors.New("trust changed flag")
	}
	quarantined, e := groupUint(a[2])
	if e != nil {
		return nil, e
	}
	return GroupTrustVerdict{Version: version, Changed: changed == 1, Quarantined: quarantined}, nil
}
func (c *Client) TrustSender(ctx context.Context, group id.ID, leaf uint64, user id.ID, history, heads []byte, now uint64) (GroupTrustVerdict, error) {
	if group.IsZero() || leaf > math.MaxUint32 || user.IsZero() || groupInputBody(history, 4<<20) != nil || groupInputBody(heads, 4<<20) != nil || now > math.MaxInt64 {
		return GroupTrustVerdict{}, errors.New("invalid trust sender input")
	}
	v, e := c.groupCall(ctx, []any{uint64(14), group, leaf, user, groupBodyPayload(history), groupBodyPayload(heads), now}, true, groupWrapped(decodeTrustVerdict))
	if e != nil {
		return GroupTrustVerdict{}, e
	}
	return resultAs[GroupTrustVerdict](v)
}
func (c *Client) ProposedAdds(ctx context.Context, group id.ID, proposals []byte) ([]GroupProposedAdd, error) {
	if group.IsZero() || validateProposalBody(proposals) != nil {
		return nil, errors.New("invalid proposed adds input")
	}
	v, e := c.groupCall(ctx, []any{uint64(15), group, groupBodyPayload(proposals)}, true, groupWrapped(func(b []byte) (any, error) {
		a, e := groupBodyArray(b)
		if e != nil {
			return nil, e
		}
		out := make([]GroupProposedAdd, 0, len(a))
		var prev uint64
		for _, x := range a {
			r, ok := x.([]any)
			if !ok || len(r) != 3 {
				return nil, errors.New("proposed add shape")
			}
			index, e := groupUint(r[0])
			if e != nil || index > math.MaxUint32 || len(out) > 0 && index <= prev {
				return nil, errors.New("proposed add index")
			}
			user, e := groupID(r[1])
			if e != nil {
				return nil, e
			}
			version, e := groupUint(r[2])
			if e != nil {
				return nil, e
			}
			out = append(out, GroupProposedAdd{uint32(index), user, version})
			prev = index
		}
		return out, nil
	}))
	if e != nil {
		return nil, e
	}
	return resultAs[[]GroupProposedAdd](v)
}
func (c *Client) TrustProposedAdd(ctx context.Context, group id.ID, proposals []byte, index uint64, user id.ID, history, heads []byte, now uint64) (GroupTrustVerdict, error) {
	if group.IsZero() || validateProposalBody(proposals) != nil || index > math.MaxUint32 || user.IsZero() || groupInputBody(history, 4<<20) != nil || groupInputBody(heads, 4<<20) != nil || now > math.MaxInt64 {
		return GroupTrustVerdict{}, errors.New("invalid proposed add trust input")
	}
	v, e := c.groupCall(ctx, []any{uint64(16), group, groupBodyPayload(proposals), index, user, groupBodyPayload(history), groupBodyPayload(heads), now}, true, groupWrapped(decodeTrustVerdict))
	if e != nil {
		return GroupTrustVerdict{}, e
	}
	return resultAs[GroupTrustVerdict](v)
}
func (c *Client) PendingLeaves(ctx context.Context) ([]GroupPendingLeaf, error) {
	v, e := c.groupCall(ctx, []any{uint64(17)}, false, groupWrapped(func(b []byte) (any, error) {
		a, e := groupBodyArray(b)
		if e != nil {
			return nil, e
		}
		out := make([]GroupPendingLeaf, 0, len(a))
		for _, x := range a {
			r, ok := x.([]any)
			if !ok || len(r) != 5 {
				return nil, errors.New("pending leaf shape")
			}
			g, e := groupID(r[0])
			if e != nil {
				return nil, e
			}
			leaf, e := groupUint(r[1])
			if e != nil || leaf > math.MaxUint32 {
				return nil, errors.New("pending leaf index")
			}
			u, e := groupID(r[2])
			if e != nil {
				return nil, e
			}
			version, e := groupUint(r[3])
			if e != nil {
				return nil, e
			}
			reason, e := groupText(r[4], 64)
			if e != nil || reason != "unknown" && reason != "ready-refresh" && reason != "changed" {
				return nil, errors.New("pending leaf reason")
			}
			out = append(out, GroupPendingLeaf{g, uint32(leaf), u, version, reason})
		}
		return out, nil
	}))
	if e != nil {
		return nil, e
	}
	return resultAs[[]GroupPendingLeaf](v)
}
func decodeGroupEvents(b []byte, binding Binding, after uint64) (any, error) {
	if len(b) > maxEventBody {
		return nil, errors.New("event page exceeds core limit")
	}
	a, e := groupBodyArray(b)
	if e != nil || len(a) != 3 {
		return nil, errors.New("event page shape")
	}
	rows, ok := a[0].([]any)
	if !ok || len(rows) > 100 {
		return nil, errors.New("event rows")
	}
	out := GroupEventPage{Events: make([]GroupEvent, 0, len(rows))}
	last := after
	for _, x := range rows {
		r, ok := x.([]any)
		if !ok || len(r) != 17 {
			return nil, errors.New("event row shape")
		}
		var v GroupEvent
		if v.DeliveryID, e = groupUint(r[0]); e != nil || v.DeliveryID <= last {
			return nil, errors.New("event delivery order")
		}
		last = v.DeliveryID
		if v.Group, e = groupID(r[1]); e != nil {
			return nil, e
		}
		if v.Community, e = groupID(r[2]); e != nil || v.Community != binding.Community {
			return nil, errors.New("event community")
		}
		if v.Channel, e = groupID(r[3]); e != nil {
			return nil, e
		}
		if v.SourceSequence, e = groupUint(r[4]); e != nil || v.SourceSequence == 0 {
			return nil, errors.New("event source sequence")
		}
		if v.Epoch, e = groupUint(r[5]); e != nil {
			return nil, e
		}
		if v.ReceivedAt, e = groupUint(r[6]); e != nil {
			return nil, e
		}
		if v.SenderUser, e = groupID(r[7]); e != nil {
			return nil, e
		}
		if v.SenderDevice, e = groupID(r[8]); e != nil {
			return nil, e
		}
		if v.SenderKind, e = groupUint(r[9]); e != nil || v.SenderKind > 1 {
			return nil, errors.New("event sender kind")
		}
		if v.SenderTier, e = groupUint(r[10]); e != nil || v.SenderTier > 1 {
			return nil, errors.New("event sender tier")
		}
		leaf, e := groupUint(r[11])
		if e != nil || leaf > math.MaxUint32 {
			return nil, errors.New("event sender leaf")
		}
		v.SenderLeaf = uint32(leaf)
		if v.Message, e = groupID(r[12]); e != nil {
			return nil, e
		}
		if v.EnvelopeType, e = groupUint(r[13]); e != nil || v.EnvelopeType > 6 {
			return nil, errors.New("event envelope type")
		}
		if r[14] != nil {
			s, ok := r[14].(string)
			if !ok || !utf8.ValidString(s) || len([]byte(s)) > 4000 {
				return nil, errors.New("event body")
			}
			v.Body = &s
		}
		if v.ReplyTo, e = nullableGroupID(r[15]); e != nil {
			return nil, e
		}
		if v.Thread, e = nullableGroupID(r[16]); e != nil {
			return nil, e
		}
		out.Events = append(out.Events, v)
	}
	if out.Next, e = groupUint(a[1]); e != nil {
		return nil, e
	}
	if out.HighWater, e = groupUint(a[2]); e != nil || out.Next < after {
		return nil, errors.New("event cursors")
	}
	// The core reports a high water never below its durable ack (dilla-core bot_events.rs event_high_water), so
	// next <= high_water is an invariant here, and the cross-language vectors in protocol/vectors/bot.json pin it.
	if out.Next > out.HighWater {
		return nil, errEventCursorAboveHighWater
	}
	if len(out.Events) > 0 && out.Next < last {
		return nil, errors.New("event cursor behind page")
	}
	return out, nil
}

var errEventCursorAboveHighWater = errors.New("event cursors: next above high water")

// ReadGroupEvents is not a mutation: the core read's only write is the idempotent redaction of rows that fail their
// rechecks, so a page Go refuses leaves no unknown outcome. That poll fails with a fixed detail token
// (DecodeDetail) and the helper stays up; before v0.1.0-bot2.3 every refused page closed it.
func (c *Client) ReadGroupEvents(ctx context.Context, after, limit uint64) (GroupEventPage, error) {
	if after > math.MaxInt64 || limit < 1 || limit > 100 {
		return GroupEventPage{}, errors.New("invalid event page request")
	}
	decode := groupWrapped(func(b []byte) (any, error) { return decodeGroupEvents(b, c.binding, after) })
	v, e := c.groupCall(ctx, []any{uint64(18), after, limit}, false, func(a []any) (any, error) {
		v, e := decode(a)
		if e != nil {
			detail := EventsPageInvalid
			if errors.Is(e, errEventCursorAboveHighWater) {
				detail = EventsCursorAboveHighWater
			}
			return nil, NewDetailError(detail, e)
		}
		return v, nil
	})
	if e != nil {
		return GroupEventPage{}, e
	}
	return resultAs[GroupEventPage](v)
}
func (c *Client) AcknowledgeGroupEvents(ctx context.Context, through uint64) error {
	if through > math.MaxInt64 {
		return errors.New("invalid event acknowledgement")
	}
	_, e := c.groupCall(ctx, []any{uint64(19), through}, true, voidResult)
	return e
}
func (c *Client) GroupIntentResult(ctx context.Context, operation id.ID) (*GroupIntentResult, error) {
	if operation.IsZero() {
		return nil, errors.New("invalid intent operation")
	}
	v, e := c.groupCall(ctx, []any{uint64(20), operation}, false, groupWrapped(func(b []byte) (any, error) {
		a, e := groupBodyArray(b)
		if e != nil {
			return nil, e
		}
		if len(a) == 1 && a[0] == nil {
			return (*GroupIntentResult)(nil), nil
		}
		if len(a) != 4 {
			return nil, errors.New("intent result shape")
		}
		kind, e := groupUint(a[0])
		if e != nil || kind != 2 && kind != 5 && kind != 9 {
			return nil, errors.New("intent kind")
		}
		g, e := groupID(a[1])
		if e != nil {
			return nil, e
		}
		state, e := groupUint(a[2])
		if e != nil || state > 2 {
			return nil, errors.New("intent state")
		}
		var raw []byte
		if a[3] != nil {
			raw, e = groupBytes(a[3], 1, maxGroupBody)
			if e != nil {
				return nil, e
			}
		}
		if state == 0 && raw == nil || state != 0 && raw != nil {
			return nil, errors.New("intent resolution body")
		}
		if raw != nil {
			switch kind {
			case 2:
				if _, e = decodeGroupCreate(raw, g, c.binding, nil); e != nil {
					return nil, e
				}
			case 5:
				if _, e = decodeExternalJoin(raw); e != nil {
					return nil, e
				}
			case 9:
				if _, e = decodeCommitBuild(raw); e != nil {
					return nil, e
				}
			}
		}
		return &GroupIntentResult{Kind: kind, Group: g, State: state, Result: raw}, nil
	}))
	if e != nil {
		return nil, e
	}
	return resultAs[*GroupIntentResult](v)
}
