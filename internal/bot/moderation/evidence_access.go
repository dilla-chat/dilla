package moderation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	evidenceStateArea                 = uint64(9)
	memberRegistrationCatchupStateKey = "dilla.bot.member-registration-catchup.v1"
	evidencePurgeCursorArea           = uint64(8)
	evidenceVersion                   = uint64(3)
	evidenceScanLimit                 = uint64(100)
	evidenceCursorMax                 = 128
	evidencePurgeCursorVersion        = uint64(1)
	permViewAuditLog                  = uint64(1 << 17)
	maxEvidenceBytes                  = 4 << 10
	MaxEvidenceReasonBytes            = 1024
	EvidenceRetention                 = 90 * 24 * time.Hour
)

var evidencePurgeCursorKey = []byte("dilla.bot.moderation.evidence.purge-cursor.v1")

var (
	ErrEvidenceAccess   = errors.New("moderation evidence unavailable")
	ErrEvidenceWrite    = errors.New("moderation evidence could not be stored")
	ErrEvidenceConflict = errors.New("moderation evidence operation conflict")
	errEvidenceExpired  = errors.New("moderation evidence expired")
)

type evidenceAuthorityClient interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
}

// Evidence is sensitive content held only in the helper's encrypted state.
// Read access is checked against the actor's current server permissions each time.
type Evidence struct {
	ID                         id.ID
	OperationID                id.ID
	MessageID                  id.ID
	AcceptedActionID           id.ID
	AcceptedRevision           uint64
	ActorUserID, ActorDeviceID id.ID
	GroupID                    id.ID
	TargetSequence             uint64
	TargetUserID               id.ID
	Action                     Action
	Rule                       Rule
	Reason                     string
	Excerpt                    string
	CreatedAt                  time.Time
	ExpiresAt                  time.Time
}

// EvidenceInput must represent either a runtime-generated automatic action or
// an authoritative accepted manual removal. Callers must not construct manual
// receipt fields from an unverified request.
type EvidenceInput struct {
	OperationID                id.ID
	MessageID                  id.ID
	Action                     Action
	Rule                       Rule
	Reason                     string
	Excerpt                    string
	CreatedAt                  time.Time
	AcceptedActionID           id.ID
	AcceptedRevision           uint64
	ActorUserID, ActorDeviceID id.ID
	GroupID                    id.ID
	TargetSequence             uint64
	TargetUserID               id.ID
}

// EvidenceWriter stores evidence under the installation's encrypted app state.
type EvidenceWriter struct {
	state   configStateClient
	binding native.Binding
}

type evidenceRetentionState interface {
	StateScan(context.Context, uint64, []byte, uint64) (native.StatePage, error)
}

func encodeEvidencePurgeCursor(cursor []byte) ([]byte, error) {
	if len(cursor) == 0 || len(cursor) > evidenceCursorMax {
		return nil, ErrEvidenceWrite
	}
	return cborx.Marshal([]any{evidencePurgeCursorVersion, cursor})
}

func decodeEvidencePurgeCursor(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > evidenceCursorMax+16 || cborx.CheckDeterministic(raw) != nil {
		return nil, ErrEvidenceWrite
	}
	var fields []any
	if cborx.Unmarshal(raw, &fields) != nil || len(fields) != 2 || fields[0] != evidencePurgeCursorVersion {
		return nil, ErrEvidenceWrite
	}
	cursor, ok := fields[1].([]byte)
	if !ok || len(cursor) == 0 || len(cursor) > evidenceCursorMax {
		return nil, ErrEvidenceWrite
	}
	return append([]byte(nil), cursor...), nil
}

func NewEvidenceWriter(state configStateClient) *EvidenceWriter {
	if state == nil {
		return &EvidenceWriter{}
	}
	return &EvidenceWriter{state: state, binding: state.Binding()}
}

// BoundTo reports whether this writer belongs to the expected bot installation
// and community. It lets interaction wiring fail closed before resolving a
// human action receipt if dependencies were accidentally crossed.
func (w *EvidenceWriter) BoundTo(installation, community id.ID) bool {
	return w != nil && validConfigBinding(w.binding) && !installation.IsZero() && !community.IsZero() &&
		w.binding.Installation == installation && w.binding.Community == community
}

// Save is idempotent by OperationID. The record expires no later than 90 days
// after CreatedAt; retries with different contents under the same ID conflict.
func (w *EvidenceWriter) Save(ctx context.Context, input EvidenceInput) error {
	manual := input.Rule == RuleManual
	if w == nil || w.state == nil || !validConfigBinding(w.binding) || input.OperationID.IsZero() || (!manual && input.MessageID.IsZero()) ||
		!validEvidenceAction(input.Action) || !validEvidenceRule(input.Rule) || len(input.Reason) == 0 || len(input.Reason) > MaxEvidenceReasonBytes ||
		strings.TrimSpace(input.Reason) == "" || !utf8.ValidString(input.Reason) || len(input.Excerpt) > ExcerptMaxBytes || !utf8.ValidString(input.Excerpt) || input.CreatedAt.IsZero() {
		return ErrEvidenceWrite
	}
	if manual {
		validRemoval := input.Action == ActionRemove && !input.AcceptedActionID.IsZero() && input.AcceptedRevision > 0 &&
			!input.ActorUserID.IsZero() && !input.ActorDeviceID.IsZero() && !input.GroupID.IsZero() && input.TargetSequence > 0 && input.TargetUserID.IsZero()
		validWarning := input.Action == ActionWarn && !input.AcceptedActionID.IsZero() && input.AcceptedRevision > 0 &&
			!input.ActorUserID.IsZero() && !input.ActorDeviceID.IsZero() && !input.TargetUserID.IsZero() && input.GroupID.IsZero() &&
			input.TargetSequence == 0 && input.MessageID.IsZero() && input.Excerpt == ""
		if !validRemoval && !validWarning {
			return ErrEvidenceWrite
		}
	} else if !input.AcceptedActionID.IsZero() || input.AcceptedRevision != 0 || !input.ActorUserID.IsZero() || !input.ActorDeviceID.IsZero() || !input.GroupID.IsZero() || input.TargetSequence != 0 || !input.TargetUserID.IsZero() {
		return ErrEvidenceWrite
	}
	now := time.Now().UTC()
	created := input.CreatedAt.UTC().Truncate(time.Second)
	if !withinEvidenceRetention(input.CreatedAt, now) {
		return ErrEvidenceWrite
	}
	input.CreatedAt = created
	body, err := encodeEvidence(w.binding, input)
	if err != nil || len(body) > maxEvidenceBytes {
		return ErrEvidenceWrite
	}
	key := evidenceKey(w.binding, input.OperationID)
	current, err := w.state.StateGet(ctx, evidenceStateArea, key)
	if err != nil {
		return ErrEvidenceWrite
	}
	if current.Revision == 0 {
		legacy, legacyErr := w.state.StateGet(ctx, evidenceStateArea, legacyEvidenceKey(w.binding, input.OperationID))
		if legacyErr == nil && legacy.Revision > 0 {
			if evidenceMatches(legacy, w.binding, input) {
				return nil
			}
			return ErrEvidenceConflict
		}
	}
	if current.Revision != 0 {
		if evidenceMatches(current, w.binding, input) {
			return nil
		}
		return ErrEvidenceConflict
	}
	_, err = w.state.StateTransaction(ctx, input.OperationID, []native.StateChange{{Area: evidenceStateArea, Key: key, ExpectedRevision: 0, Value: body}})
	if err == nil {
		return nil
	}
	// A lost response or concurrent retry may have committed. Resolve by reading
	// the same operation key; never expose helper errors that can carry plaintext.
	current, readErr := w.state.StateGet(ctx, evidenceStateArea, key)
	if readErr == nil && current.Revision != 0 && evidenceMatches(current, w.binding, input) {
		return nil
	}
	return ErrEvidenceConflict
}

func withinEvidenceRetention(createdAt, now time.Time) bool {
	if createdAt.IsZero() {
		return false
	}
	age := now.UTC().Sub(createdAt.UTC())
	return age <= EvidenceRetention && age >= -5*time.Minute
}

// EvidenceReader reads one installation-bound evidence item after resolving
// the requesting actor's current view_audit_log permission from the server.
type EvidenceReader struct {
	state     configStateClient
	authority evidenceAuthorityClient
	binding   native.Binding
}

func NewEvidenceReader(state configStateClient, authority evidenceAuthorityClient) *EvidenceReader {
	if state == nil {
		return &EvidenceReader{}
	}
	return &EvidenceReader{state: state, authority: authority, binding: state.Binding()}
}

// Read performs a fresh authority lookup before touching encrypted state.
// Any missing dependency, lookup failure, absent actor, or permission denial
// returns the same unavailable error so callers cannot infer evidence metadata.
func (r *EvidenceReader) Read(ctx context.Context, actor, evidenceID id.ID) (Evidence, error) {
	if r == nil || r.state == nil || r.authority == nil || !validConfigBinding(r.binding) || actor.IsZero() || evidenceID.IsZero() {
		return Evidence{}, ErrEvidenceAccess
	}
	resolved, present, err := r.authority.BotActorAuthority(ctx, r.binding.Community, actor)
	if err != nil || !present || resolved.EffectiveBits&permViewAuditLog == 0 {
		return Evidence{}, ErrEvidenceAccess
	}
	value, err := r.state.StateGet(ctx, evidenceStateArea, evidenceKey(r.binding, evidenceID))
	if err == nil && value.Revision == 0 {
		value, err = r.state.StateGet(ctx, evidenceStateArea, legacyEvidenceKey(r.binding, evidenceID))
	}
	if err != nil || value.Tombstone || value.Revision == 0 || len(value.Value) == 0 || len(value.Value) > maxEvidenceBytes || cborx.ExpectMajor(value.Value, cborx.MajorArray) != nil {
		return Evidence{}, ErrEvidenceAccess
	}
	var row []any
	if cborx.Unmarshal(value.Value, &row) != nil || (len(row) != 13 && len(row) != 19 && len(row) != 20) {
		return Evidence{}, ErrEvidenceAccess
	}
	version, vok := row[0].(uint64)
	instance, iok := configID(row[1])
	community, cok := configID(row[2])
	installation, inok := configID(row[3])
	operationID, ook := configID(row[4])
	storedID, eok := configID(row[5])
	messageID, mok := configID(row[6])
	actionText, aok := row[7].(string)
	ruleText, rok := row[8].(string)
	reason, nok := row[9].(string)
	excerpt, xok := row[10].(string)
	created, tok := row[11].(uint64)
	expires, exok := row[12].(uint64)
	var acceptedActionID, actorUserID, actorDeviceID id.ID
	var acceptedRevision uint64
	var groupID id.ID
	var targetSequence uint64
	var targetUserID id.ID
	var metadataOK bool
	if len(row) >= 19 {
		acceptedActionID, metadataOK = configID(row[13])
		var ok bool
		acceptedRevision, ok = row[14].(uint64)
		metadataOK = metadataOK && ok
		actorUserID, ok = configID(row[15])
		metadataOK = metadataOK && ok
		actorDeviceID, ok = configID(row[16])
		metadataOK = metadataOK && ok
		groupID, ok = configID(row[17])
		metadataOK = metadataOK && ok
		targetSequence, ok = row[18].(uint64)
		metadataOK = metadataOK && ok
		if len(row) == 20 {
			targetUserID, ok = configID(row[19])
			metadataOK = metadataOK && ok
		}
	} else {
		metadataOK = version == 1
	}
	if !vok || (version != 1 && version != 2 && version != evidenceVersion) || !iok || !cok || !inok || !eok || !mok ||
		!ook || instance != r.binding.Instance || community != r.binding.Community || installation != r.binding.Installation || storedID != evidenceID || operationID != evidenceID ||
		instance.IsZero() || (messageID.IsZero() && Rule(ruleText) != RuleManual) || !aok || !rok || !validEvidenceAction(Action(actionText)) || !validEvidenceRule(Rule(ruleText)) || !nok || len(reason) == 0 || len(reason) > MaxEvidenceReasonBytes || !utf8.ValidString(reason) || !xok || len(excerpt) > ExcerptMaxBytes || !utf8.ValidString(excerpt) || !tok || !exok || created == 0 || created > math.MaxInt64 ||
		!metadataOK || (len(row) == 13 && version != 1) || (len(row) == 19 && version != 2) || (len(row) == 20 && version != 3) ||
		(Rule(ruleText) == RuleManual && (acceptedActionID.IsZero() || acceptedRevision == 0 || actorUserID.IsZero() || actorDeviceID.IsZero() ||
			(Action(actionText) == ActionRemove && (len(row) < 19 || groupID.IsZero() || targetSequence == 0 || !targetUserID.IsZero())) ||
			(Action(actionText) == ActionWarn && (len(row) != 20 || !groupID.IsZero() || targetSequence != 0 || targetUserID.IsZero() || !messageID.IsZero() || excerpt != "")) ||
			(Action(actionText) != ActionRemove && Action(actionText) != ActionWarn))) ||
		(Rule(ruleText) != RuleManual && (!acceptedActionID.IsZero() || acceptedRevision != 0 || !actorUserID.IsZero() || !actorDeviceID.IsZero() || !groupID.IsZero() || targetSequence != 0 || !targetUserID.IsZero())) {
		return Evidence{}, ErrEvidenceAccess
	}
	createdAt := time.Unix(int64(created), 0).UTC()
	expiresAt := createdAt.Add(EvidenceRetention)
	expiresUnix := expiresAt.Unix()
	if expiresUnix <= 0 || expires != uint64(expiresUnix) {
		return Evidence{}, ErrEvidenceAccess
	}
	if !time.Now().UTC().Before(expiresAt) {
		return Evidence{}, fmt.Errorf("%w: %w", ErrEvidenceAccess, errEvidenceExpired)
	}
	return Evidence{ID: storedID, OperationID: operationID, MessageID: messageID, AcceptedActionID: acceptedActionID, AcceptedRevision: acceptedRevision,
		ActorUserID: actorUserID, ActorDeviceID: actorDeviceID, GroupID: groupID, TargetSequence: targetSequence, TargetUserID: targetUserID,
		Action: Action(actionText), Rule: Rule(ruleText), Reason: reason, Excerpt: excerpt,
		CreatedAt: createdAt, ExpiresAt: expiresAt}, nil
}

// List returns a deterministic, bounded page of encrypted evidence. The scan
// cursor is the last consumed native state key; callers must bind it to the
// installation/community before returning it to a client. Authorization is
// resolved before scanning and again by Read for each returned record.
func (r *EvidenceReader) List(ctx context.Context, actor id.ID, after []byte, limit uint64) ([]Evidence, []byte, error) {
	if r == nil || r.state == nil || r.authority == nil || !validConfigBinding(r.binding) || actor.IsZero() ||
		limit == 0 || limit > 8 || after != nil && (len(after) == 0 || len(after) > evidenceCursorMax) {
		return nil, nil, ErrEvidenceAccess
	}
	resolved, present, err := r.authority.BotActorAuthority(ctx, r.binding.Community, actor)
	if err != nil || !present || resolved.EffectiveBits&permViewAuditLog == 0 {
		return nil, nil, ErrEvidenceAccess
	}
	scanner, ok := any(r.state).(evidenceRetentionState)
	if !ok {
		return nil, nil, ErrEvidenceAccess
	}
	page, err := scanner.StateScan(ctx, evidenceStateArea, after, evidenceScanLimit)
	if err != nil || len(page.Entries) > int(evidenceScanLimit) || len(page.Next) > evidenceCursorMax ||
		len(page.Next) > 0 && bytes.Equal(page.Next, after) {
		return nil, nil, ErrEvidenceAccess
	}
	rows := make([]Evidence, 0, limit)
	var consumed []byte
	for i, entry := range page.Entries {
		if len(entry.Key) == 0 || len(entry.Key) > evidenceCursorMax {
			return nil, nil, ErrEvidenceAccess
		}
		consumed = append(consumed[:0], entry.Key...)
		if isEvidenceAuxiliaryKey(entry.Key) {
			continue
		}
		if entry.Tombstone || entry.Revision == 0 {
			continue
		}
		var stored []any
		if len(entry.Value) == 0 || len(entry.Value) > maxEvidenceBytes || cborx.Unmarshal(entry.Value, &stored) != nil || (len(stored) != 13 && len(stored) != 19 && len(stored) != 20) {
			return nil, nil, ErrEvidenceAccess
		}
		evidenceID, valid := configID(stored[5])
		if !valid || evidenceID.IsZero() ||
			!bytes.Equal(entry.Key, evidenceKey(r.binding, evidenceID)) && !bytes.Equal(entry.Key, legacyEvidenceKey(r.binding, evidenceID)) {
			return nil, nil, ErrEvidenceAccess
		}
		row, readErr := r.Read(ctx, actor, evidenceID)
		if errors.Is(readErr, errEvidenceExpired) {
			continue
		}
		if readErr != nil {
			return nil, nil, ErrEvidenceAccess
		}
		if row.Rule == RuleManual {
			rows = append(rows, row)
		}
		if uint64(len(rows)) == limit {
			if i < len(page.Entries)-1 {
				return rows, append([]byte(nil), consumed...), nil
			}
			if len(page.Next) > 0 {
				return rows, append([]byte(nil), page.Next...), nil
			}
			return rows, nil, nil
		}
	}
	if len(page.Next) > 0 {
		return rows, append([]byte(nil), page.Next...), nil
	}
	return rows, nil, nil
}

func encodeEvidence(binding native.Binding, input EvidenceInput) ([]byte, error) {
	expires := input.CreatedAt.Add(EvidenceRetention)
	createdUnix, expiresUnix := input.CreatedAt.Unix(), expires.Unix()
	if createdUnix <= 0 || expiresUnix <= 0 {
		return nil, ErrEvidenceWrite
	}
	return cborx.Marshal([]any{evidenceVersion, binding.Instance, binding.Community, binding.Installation, input.OperationID, input.OperationID,
		input.MessageID, string(input.Action), string(input.Rule), input.Reason, input.Excerpt, uint64(createdUnix), uint64(expiresUnix),
		input.AcceptedActionID, input.AcceptedRevision, input.ActorUserID, input.ActorDeviceID, input.GroupID, input.TargetSequence, input.TargetUserID})
}

func evidenceMatches(value native.StateValue, binding native.Binding, input EvidenceInput) bool {
	if value.Tombstone || value.Revision == 0 || len(value.Value) == 0 || len(value.Value) > maxEvidenceBytes {
		return false
	}
	var row []any
	if cborx.Unmarshal(value.Value, &row) != nil || (len(row) != 13 && len(row) != 19 && len(row) != 20) {
		return false
	}
	instance, iok := configID(row[1])
	community, cok := configID(row[2])
	installation, inok := configID(row[3])
	operation, ook := configID(row[4])
	storedID, eok := configID(row[5])
	message, mok := configID(row[6])
	created, tok := row[11].(uint64)
	expires, exok := row[12].(uint64)
	var acceptedActionID, actorUserID, actorDeviceID id.ID
	var acceptedRevision uint64
	var groupID id.ID
	var targetSequence uint64
	var targetUserID id.ID
	var metadataOK bool
	if len(row) == 13 {
		metadataOK = row[0] == uint64(1) && input.AcceptedActionID.IsZero() && input.AcceptedRevision == 0 && input.ActorUserID.IsZero() && input.ActorDeviceID.IsZero()
	} else {
		acceptedActionID, metadataOK = configID(row[13])
		var ok bool
		acceptedRevision, ok = row[14].(uint64)
		metadataOK = metadataOK && ok
		actorUserID, ok = configID(row[15])
		metadataOK = metadataOK && ok
		actorDeviceID, ok = configID(row[16])
		metadataOK = metadataOK && ok
		groupID, ok = configID(row[17])
		metadataOK = metadataOK && ok
		targetSequence, ok = row[18].(uint64)
		metadataOK = metadataOK && ok
		if len(row) == 20 {
			targetUserID, ok = configID(row[19])
			metadataOK = metadataOK && ok
		}
	}
	createdUnix, expiresUnix := input.CreatedAt.Unix(), input.CreatedAt.Add(EvidenceRetention).Unix()
	return createdUnix > 0 && expiresUnix > 0 && iok && cok && inok && ook && eok && mok && tok && exok && metadataOK &&
		instance == binding.Instance && community == binding.Community && installation == binding.Installation &&
		operation == input.OperationID && storedID == input.OperationID && message == input.MessageID &&
		((len(row) == 13 && row[0] == uint64(1)) || (len(row) == 19 && row[0] == uint64(2)) || (len(row) == 20 && row[0] == evidenceVersion)) && row[7] == string(input.Action) && row[8] == string(input.Rule) &&
		row[9] == input.Reason && row[10] == input.Excerpt && created == uint64(createdUnix) && expires == uint64(expiresUnix) &&
		(len(row) == 13 || acceptedActionID == input.AcceptedActionID && acceptedRevision == input.AcceptedRevision && actorUserID == input.ActorUserID && actorDeviceID == input.ActorDeviceID && groupID == input.GroupID && targetSequence == input.TargetSequence && (len(row) == 19 || targetUserID == input.TargetUserID))
}

func validEvidenceAction(action Action) bool {
	return action == ActionRemove || action == ActionWarn || action == ActionTimeout
}

func validEvidenceRule(rule Rule) bool {
	return validRule(rule) || rule == RuleManual
}

func evidenceKey(binding native.Binding, evidenceID id.ID) []byte {
	scope := make([]byte, 0, 64)
	scope = append(scope, binding.Instance[:]...)
	scope = append(scope, binding.Community[:]...)
	scope = append(scope, binding.Installation[:]...)
	scope = append(scope, evidenceID[:]...)
	sum := sha256.Sum256(scope)
	return []byte("ev2-" + hex.EncodeToString(sum[:]))
}

func legacyEvidenceKey(binding native.Binding, evidenceID id.ID) []byte {
	return []byte(fmt.Sprintf("dilla.bot.moderation.evidence.v1/%x/%x/%x/%x", binding.Instance, binding.Community, binding.Installation, evidenceID))
}

// PurgeExpired removes expired evidence from at most one native scan page.
// Its caller can resume from the returned opaque state key.
func (w *EvidenceWriter) PurgeExpired(ctx context.Context, after []byte) (int, []byte, error) {
	if w == nil || w.state == nil || ctx == nil || !validConfigBinding(w.binding) ||
		after != nil && (len(after) == 0 || len(after) > evidenceCursorMax) {
		return 0, nil, ErrEvidenceWrite
	}
	scanner, ok := any(w.state).(evidenceRetentionState)
	if !ok {
		return 0, nil, ErrEvidenceWrite
	}
	page, err := scanner.StateScan(ctx, evidenceStateArea, after, evidenceScanLimit)
	if err != nil {
		return 0, nil, ErrEvidenceWrite
	}
	now := time.Now().UTC().Unix()
	removed := 0
	for _, entry := range page.Entries {
		if isEvidenceAuxiliaryKey(entry.Key) {
			continue
		}
		if entry.Tombstone {
			continue
		}
		expires, decodeErr := evidenceExpiry(entry.Value, w.binding, entry.Key)
		if decodeErr != nil {
			return removed, page.Next, ErrEvidenceWrite
		}
		if expires > now {
			continue
		}
		_, txErr := w.state.StateTransaction(ctx, id.New(), []native.StateChange{{Area: evidenceStateArea, Key: entry.Key, ExpectedRevision: entry.Revision}})
		if txErr == nil {
			removed++
			continue
		}
		actual, readErr := w.state.StateGet(ctx, evidenceStateArea, entry.Key)
		if readErr == nil && actual.Tombstone && actual.Revision == entry.Revision+1 {
			removed++
			continue
		}
		return removed, page.Next, ErrEvidenceWrite
	}
	if len(page.Next) > evidenceCursorMax {
		return removed, nil, ErrEvidenceWrite
	}
	return removed, append([]byte(nil), page.Next...), nil
}

// PurgeExpiredResumable processes one scan page and persists its cursor in a
// separate encrypted app-state key so maintenance can continue after restart.
func (w *EvidenceWriter) PurgeExpiredResumable(ctx context.Context) (int, error) {
	if w == nil || w.state == nil || ctx == nil {
		return 0, ErrEvidenceWrite
	}
	cursor, err := w.state.StateGet(ctx, evidencePurgeCursorArea, evidencePurgeCursorKey)
	if err != nil {
		return 0, ErrEvidenceWrite
	}
	var after []byte
	if cursor.Revision > 0 && !cursor.Tombstone {
		after, err = decodeEvidencePurgeCursor(cursor.Value)
		if err != nil {
			return 0, ErrEvidenceWrite
		}
	}
	removed, next, err := w.PurgeExpired(ctx, after)
	if err != nil {
		return removed, err
	}
	if next == nil && (cursor.Revision == 0 || cursor.Tombstone) || next != nil && !cursor.Tombstone && bytes.Equal(next, after) {
		return removed, nil
	}
	var value []byte
	if next != nil {
		value, err = encodeEvidencePurgeCursor(next)
		if err != nil {
			return removed, ErrEvidenceWrite
		}
	}
	_, txErr := w.state.StateTransaction(ctx, id.New(), []native.StateChange{{Area: evidencePurgeCursorArea, Key: evidencePurgeCursorKey,
		ExpectedRevision: cursor.Revision, Value: value}})
	if txErr == nil {
		return removed, nil
	}
	actual, readErr := w.state.StateGet(ctx, evidencePurgeCursorArea, evidencePurgeCursorKey)
	if readErr == nil && actual.Revision == cursor.Revision+1 && actual.Tombstone == (next == nil) && bytes.Equal(actual.Value, value) {
		return removed, nil
	}
	return removed, ErrEvidenceWrite
}

func evidenceExpiry(raw []byte, binding native.Binding, key []byte) (int64, error) {
	if len(raw) == 0 || len(raw) > maxEvidenceBytes || cborx.CheckDeterministic(raw) != nil {
		return 0, ErrEvidenceWrite
	}
	var row []any
	if cborx.Unmarshal(raw, &row) != nil || len(row) != 13 && len(row) != 19 && len(row) != 20 {
		return 0, ErrEvidenceWrite
	}
	version, vok := row[0].(uint64)
	instance, iok := configID(row[1])
	community, cok := configID(row[2])
	installation, nok := configID(row[3])
	operation, ook := configID(row[4])
	storedID, sok := configID(row[5])
	created, tok := row[11].(uint64)
	expires, exok := row[12].(uint64)
	if !vok || version != 1 && version != 2 && version != evidenceVersion || (version == 1 && len(row) != 13) || (version == 2 && len(row) != 19) || (version == evidenceVersion && len(row) != 20) ||
		!iok || !cok || !nok || !ook || !sok || !tok || !exok || instance != binding.Instance || community != binding.Community || installation != binding.Installation ||
		operation.IsZero() || storedID != operation || (string(key) != string(evidenceKey(binding, operation)) && string(key) != string(legacyEvidenceKey(binding, operation))) ||
		created == 0 || created > uint64(math.MaxInt64) || created > uint64(math.MaxInt64)-uint64(EvidenceRetention/time.Second) || expires != created+uint64(EvidenceRetention/time.Second) {
		return 0, ErrEvidenceWrite
	}
	if expires > math.MaxInt64 {
		return 0, ErrEvidenceWrite
	}
	return int64(expires), nil
}

// Registration progress and durable audio counters share area 9 with evidence.
// Skip only known auxiliary namespaces; unknown/corrupt evidence stays fail-closed.
func isEvidenceAuxiliaryKey(key []byte) bool {
	return string(key) == memberRegistrationCatchupStateKey || bytes.HasPrefix(key, []byte("sframe.mic.v1\x00"))
}
