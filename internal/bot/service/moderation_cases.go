package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	moderationCaseStateArea      = uint64(4)
	moderationPurgeCursorArea    = uint64(8)
	moderationCaseVersion        = uint64(3)
	moderationCaseRetention      = 90 * 24 * time.Hour
	moderationCaseScanLimit      = uint64(100)
	moderationCaseMaxValue       = 16 << 10
	moderationCaseMaxCursor      = 128
	moderationPurgeCursorVersion = uint64(1)
)

var moderationPurgeCursorKey = []byte("dilla.bot.moderation.purge-cursor.v1")

func encodeModerationPurgeCursor(cursor []byte) ([]byte, error) {
	if len(cursor) == 0 || len(cursor) > moderationCaseMaxCursor {
		return nil, ErrModerationCaseState
	}
	return cborx.Marshal([]any{moderationPurgeCursorVersion, cursor})
}

func decodeModerationPurgeCursor(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > moderationCaseMaxCursor+16 || cborx.CheckDeterministic(raw) != nil {
		return nil, ErrModerationCaseState
	}
	var fields []any
	if cborx.Unmarshal(raw, &fields) != nil || len(fields) != 2 || fields[0] != moderationPurgeCursorVersion {
		return nil, ErrModerationCaseState
	}
	cursor, ok := fields[1].([]byte)
	if !ok || len(cursor) == 0 || len(cursor) > moderationCaseMaxCursor {
		return nil, ErrModerationCaseState
	}
	return append([]byte(nil), cursor...), nil
}

var (
	ErrModerationCaseState          = errors.New("invalid moderation case state")
	ErrModerationCaseConflict       = errors.New("moderation case conflicts with existing source revision")
	ErrModerationCaseBinding        = errors.New("moderation case belongs to another installation or community")
	ErrModerationAcceptanceMismatch = errors.New("accepted moderation action does not match requested source")
)

type moderationCaseNative interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateScan(context.Context, uint64, []byte, uint64) (native.StatePage, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

// ModerationCase is an installation-bound record stored in encrypted helper
// app state. Excerpt is retained only as a bounded, explicitly supplied preview.
type ModerationCase struct {
	EnforcementRefused                        bool
	Community, Group, Channel, Actor, Message string
	SourceRevision                            uint64
	Rules                                     []moderation.Rule
	Action                                    moderation.Action
	Timeout                                   time.Duration
	Excerpt                                   string
	ReceivedAt                                time.Time
	ServerActionID                            id.ID
	ServerOperationID                         id.ID
	ServerRevision                            uint64
}

// Reason is deterministic from the matched rules and contains no message text.
func (c ModerationCase) Reason() string {
	if c.EnforcementRefused {
		c.EnforcementRefused = false
		return "enforcement refused: " + c.Reason()
	}
	if len(c.Rules) == 0 {
		return "automatic moderation"
	}
	rules := make([]string, len(c.Rules))
	for i, rule := range c.Rules {
		rules[i] = string(rule)
	}
	sort.Strings(rules)
	return "automatic moderation: " + strings.Join(rules, ", ")
}

// ModerationCaseSink reads and purges the automatic moderation cases an earlier version stored, one per source message
// revision (the moderation log's automatic list). Bot v2 Task 23 removed the producer: nothing writes new cases.
type ModerationCaseSink struct {
	native         moderationCaseNative
	evidenceWriter *moderation.EvidenceWriter
	binding        native.Binding
	now            func() time.Time
}

func NewModerationCaseSink(n moderationCaseNative, now func() time.Time) (*ModerationCaseSink, error) {
	if now == nil {
		now = time.Now
	}
	if n == nil {
		return nil, ErrModerationCaseState
	}
	b := n.Binding()
	if b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() {
		return nil, ErrModerationCaseBinding
	}
	return &ModerationCaseSink{native: n, evidenceWriter: moderation.NewEvidenceWriter(n), binding: b, now: now}, nil
}

func validModerationCaseID(value string) bool {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	return utf8.ValidString(value)
}

func moderationCaseKey(message string, revision uint64) []byte {
	base, _ := cborx.Marshal([]any{message, revision})
	sum := sha256.Sum256(base)
	return []byte("mc1-" + hex.EncodeToString(sum[:]))
}

func decodeModerationCase(raw []byte, binding native.Binding) (ModerationCase, error) {
	var a []any
	if len(raw) == 0 || len(raw) > moderationCaseMaxValue || cborx.CheckDeterministic(raw) != nil || cborx.Unmarshal(raw, &a) != nil || ((len(a) != 18 || a[0] != uint64(2)) && (len(a) != 19 || a[0] != moderationCaseVersion)) {
		return ModerationCase{}, ErrModerationCaseState
	}
	instance, iok := moderationCaseID(a[1])
	community, cok := moderationCaseID(a[2])
	installation, nok := moderationCaseID(a[3])
	if !iok || !cok || !nok {
		return ModerationCase{}, ErrModerationCaseState
	}
	if instance != binding.Instance || community != binding.Community || installation != binding.Installation {
		return ModerationCase{}, ErrModerationCaseBinding
	}
	str := func(i int) (string, bool) { v, ok := a[i].(string); return v, ok && validModerationCaseID(v) }
	c := ModerationCase{}
	if len(a) == 19 {
		flag, ok := a[18].(uint64)
		c.EnforcementRefused = flag == 1
		if !ok || flag > 1 {
			return ModerationCase{}, ErrModerationCaseState
		}
	}
	var ok bool
	if c.Community, ok = str(4); !ok || c.Community != binding.Community.String() {
		return ModerationCase{}, ErrModerationCaseBinding
	}
	if c.Group, ok = str(5); !ok {
		return ModerationCase{}, ErrModerationCaseState
	}
	if c.Channel, ok = str(6); !ok {
		return ModerationCase{}, ErrModerationCaseState
	}
	if c.Actor, ok = str(7); !ok {
		return ModerationCase{}, ErrModerationCaseState
	}
	if c.Message, ok = str(8); !ok {
		return ModerationCase{}, ErrModerationCaseState
	}
	var uok bool
	if c.SourceRevision, uok = a[9].(uint64); !uok || c.SourceRevision == 0 {
		return ModerationCase{}, ErrModerationCaseState
	}
	arr, ok := a[10].([]any)
	if !ok || len(arr) == 0 || len(arr) > 3 {
		return ModerationCase{}, ErrModerationCaseState
	}
	seen := map[moderation.Rule]bool{}
	for _, item := range arr {
		v, ok := item.(string)
		rule := moderation.Rule(v)
		if !ok || (rule != moderation.RuleRate && rule != moderation.RuleDuplicate && rule != moderation.RuleMentions) || seen[rule] {
			return ModerationCase{}, ErrModerationCaseState
		}
		seen[rule] = true
		c.Rules = append(c.Rules, rule)
	}
	action, ok := a[11].(string)
	c.Action = moderation.Action(action)
	if c.EnforcementRefused && c.Action != moderation.ActionLog {
		return ModerationCase{}, ErrModerationCaseState
	}
	if !ok || (c.Action != moderation.ActionLog && c.Action != moderation.ActionRemove && c.Action != moderation.ActionWarn && c.Action != moderation.ActionTimeout) {
		return ModerationCase{}, ErrModerationCaseState
	}
	ms, ok := a[12].(uint64)
	if !ok || ms > uint64(moderation.MaxTimeout/time.Millisecond) || c.Action == moderation.ActionTimeout && ms == 0 || c.Action != moderation.ActionTimeout && ms != 0 {
		return ModerationCase{}, ErrModerationCaseState
	}
	c.Timeout = time.Duration(ms) * time.Millisecond
	if c.Excerpt, ok = a[13].(string); !ok || !utf8.ValidString(c.Excerpt) || len(c.Excerpt) > moderation.ExcerptMaxBytes {
		return ModerationCase{}, ErrModerationCaseState
	}
	timestamp, ok := a[14].(uint64)
	if !ok || timestamp == 0 || timestamp > math.MaxInt64 {
		return ModerationCase{}, ErrModerationCaseState
	}
	c.ReceivedAt = time.UnixMilli(int64(timestamp))
	if c.ServerActionID, ok = moderationCaseID(a[15]); !ok {
		return ModerationCase{}, ErrModerationCaseState
	}
	if c.ServerRevision, ok = a[16].(uint64); !ok {
		return ModerationCase{}, ErrModerationCaseState
	}
	if c.ServerOperationID, ok = moderationCaseID(a[17]); !ok {
		return ModerationCase{}, ErrModerationCaseState
	}
	if c.Action == moderation.ActionLog {
		if !c.ServerActionID.IsZero() || !c.ServerOperationID.IsZero() || c.ServerRevision != 0 {
			return ModerationCase{}, ErrModerationCaseState
		}
	} else {
		_, groupErr := id.Parse(c.Group)
		_, messageErr := id.Parse(c.Message)
		if (c.Action != moderation.ActionRemove && c.Action != moderation.ActionWarn && c.Action != moderation.ActionTimeout) || c.ServerActionID.IsZero() || c.ServerOperationID.IsZero() || c.ServerRevision == 0 || c.ServerRevision > math.MaxInt64 ||
			groupErr != nil || messageErr != nil {
			return ModerationCase{}, ErrModerationCaseState
		}
	}
	return c, nil
}

func moderationCaseID(v any) (id.ID, bool) {
	b, ok := v.([]byte)
	if !ok || len(b) != len(id.ID{}) {
		return id.ID{}, false
	}
	var out id.ID
	copy(out[:], b)
	return out, true
}

// list is the unguarded storage primitive. Callers must use ModerationCaseReader
// so current view_audit_log permission is checked before scanning encrypted state.
func (s *ModerationCaseSink) list(ctx context.Context, after []byte, limit uint64) ([]ModerationCase, []byte, error) {
	if s == nil || ctx == nil || limit == 0 || limit > moderationCaseScanLimit || after != nil && (len(after) == 0 || len(after) > moderationCaseMaxCursor) {
		return nil, nil, ErrModerationCaseState
	}
	page, err := s.native.StateScan(ctx, moderationCaseStateArea, after, limit)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]ModerationCase, 0, len(page.Entries))
	for _, entry := range page.Entries {
		if entry.Tombstone {
			continue
		}
		row, decErr := decodeModerationCase(entry.Value, s.binding)
		if decErr != nil {
			return nil, nil, decErr
		}
		if string(entry.Key) != string(moderationCaseKey(row.Message, row.SourceRevision)) {
			return nil, nil, ErrModerationCaseState
		}
		rows = append(rows, row)
	}
	return rows, append([]byte(nil), page.Next...), nil
}

// PurgeExpired removes at most one native state page per call. Callers should
// persist the returned cursor and resume until it is nil, including after a restart.
func (s *ModerationCaseSink) PurgeExpired(ctx context.Context, after []byte) (int, []byte, error) {
	if s == nil || ctx == nil || after != nil && (len(after) == 0 || len(after) > moderationCaseMaxCursor) {
		return 0, nil, ErrModerationCaseState
	}
	page, err := s.native.StateScan(ctx, moderationCaseStateArea, after, moderationCaseScanLimit)
	if err != nil {
		return 0, nil, err
	}
	cutoff := s.now().Add(-moderationCaseRetention)
	removed := 0
	for _, entry := range page.Entries {
		if entry.Tombstone {
			continue
		}
		row, decErr := decodeModerationCase(entry.Value, s.binding)
		if decErr != nil {
			return removed, page.Next, decErr
		}
		if string(entry.Key) != string(moderationCaseKey(row.Message, row.SourceRevision)) {
			return removed, page.Next, ErrModerationCaseState
		}
		if row.ReceivedAt.After(cutoff) {
			continue
		}
		_, txErr := s.native.StateTransaction(ctx, id.New(), []native.StateChange{{Area: moderationCaseStateArea, Key: entry.Key, ExpectedRevision: entry.Revision}})
		if txErr == nil {
			removed++
			continue
		}
		actual, readErr := s.native.StateGet(ctx, moderationCaseStateArea, entry.Key)
		if readErr == nil && actual.Tombstone && actual.Revision == entry.Revision+1 {
			removed++
			continue
		}
		return removed, page.Next, fmt.Errorf("purge moderation case: %w", txErr)
	}
	return removed, append([]byte(nil), page.Next...), nil
}

// PurgeExpiredResumable removes one bounded page and stores its cursor in
// encrypted app state, so the next maintenance run can continue after restart.
// If the cursor write has an uncertain result, reading back the desired value
// makes retry safe; repeating a page is also safe because deletes use revisions.
func (s *ModerationCaseSink) PurgeExpiredResumable(ctx context.Context) (int, error) {
	if s == nil || ctx == nil {
		return 0, ErrModerationCaseState
	}
	evidenceRemoved, err := s.evidenceWriter.PurgeExpiredResumable(ctx)
	if err != nil {
		return evidenceRemoved, err
	}
	cursor, err := s.native.StateGet(ctx, moderationPurgeCursorArea, moderationPurgeCursorKey)
	if err != nil {
		return evidenceRemoved, err
	}
	var after []byte
	if cursor.Revision > 0 && !cursor.Tombstone {
		after, err = decodeModerationPurgeCursor(cursor.Value)
		if err != nil {
			return evidenceRemoved, ErrModerationCaseState
		}
	}
	removed, next, err := s.PurgeExpired(ctx, after)
	if err != nil {
		return evidenceRemoved + removed, err
	}
	if len(next) > moderationCaseMaxCursor {
		return evidenceRemoved + removed, ErrModerationCaseState
	}
	if next == nil && (cursor.Revision == 0 || cursor.Tombstone) || next != nil && !cursor.Tombstone && bytes.Equal(next, after) {
		return evidenceRemoved + removed, nil
	}
	var value []byte
	if next != nil {
		value, err = encodeModerationPurgeCursor(next)
		if err != nil {
			return evidenceRemoved + removed, ErrModerationCaseState
		}
	}
	_, txErr := s.native.StateTransaction(ctx, id.New(), []native.StateChange{{Area: moderationPurgeCursorArea, Key: moderationPurgeCursorKey,
		ExpectedRevision: cursor.Revision, Value: value}})
	if txErr == nil {
		return evidenceRemoved + removed, nil
	}
	actual, readErr := s.native.StateGet(ctx, moderationPurgeCursorArea, moderationPurgeCursorKey)
	if readErr == nil && actual.Revision == cursor.Revision+1 && actual.Tombstone == (next == nil) && bytes.Equal(actual.Value, value) {
		return evidenceRemoved + removed, nil
	}
	return evidenceRemoved + removed, fmt.Errorf("persist moderation purge cursor: %w", txErr)
}
