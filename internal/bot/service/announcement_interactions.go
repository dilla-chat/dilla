package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dilla-chat/dilla/internal/bot/announcements"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	announcementPageSize           = 8
	announcementDataLimit          = 2400
	announcementValueVersion       = 1
	maxAnnouncementInteractionText = 1800
)

// AnnouncementInteractionAuthority resolves the actor's current
// manage_community permission. Actor identity always comes from
// InteractionDispatcher's authenticated native event.
type AnnouncementInteractionAuthority interface {
	CanManageCommunity(context.Context, id.ID, id.ID) (bool, error)
}

type AnnouncementInteractions struct {
	Scheduler   *announcements.Scheduler
	Authority   AnnouncementInteractionAuthority
	CommunityID id.ID
}

// Handlers returns callable announcement handlers. Put and Delete check the
// protocol document revision in the scheduler transaction and also honor each
// schedule's own revision.
func (h AnnouncementInteractions) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	handlers := h.ReadOnlyHandlers()
	handlers[protocol.OpAnnouncementsPut] = InteractionHandlerSpec{
		RevisionInHandler: true,
		Handle:            h.put,
	}
	handlers[protocol.OpAnnouncementsDelete] = InteractionHandlerSpec{
		RevisionInHandler: true,
		Handle:            h.delete,
	}
	return handlers
}

// ReadOnlyHandlers exposes encrypted schedule listing for call sites that
// intentionally need a read-only announcement surface.
func (h AnnouncementInteractions) ReadOnlyHandlers() map[protocol.Operation]InteractionHandlerSpec {
	return map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpAnnouncementsList: {RecomputeOnReplay: true, Handle: h.list},
	}
}

func (h AnnouncementInteractions) authorize(ctx context.Context, actor InteractionActor) (bool, error) {
	if h.Scheduler == nil || h.Authority == nil || h.CommunityID.IsZero() || actor.User.IsZero() {
		return false, errors.New("missing announcement interaction dependency")
	}
	return h.Authority.CanManageCommunity(ctx, actor.User, h.CommunityID)
}

func (h AnnouncementInteractions) list(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if request.CommunityID != h.CommunityID {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	allowed, err := h.authorize(ctx, actor)
	if err != nil {
		return InteractionResult{}, err
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	if len(request.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	offset, ok := request.Args[0].(uint64)
	if !ok || offset > math.MaxInt32 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	rows, revision, err := h.Scheduler.ListAtRevision(ctx)
	if err != nil {
		return InteractionResult{}, err
	}
	start := int(offset)
	if start > len(rows) {
		start = len(rows)
	}
	page := make([]any, 0, announcementPageSize)
	var data []byte
	for i := start; i < len(rows) && len(page) < announcementPageSize; i++ {
		candidate := append(page, announcementSummary(rows[i]))
		candidateData, marshalErr := cborx.Marshal([]any{uint64(announcementValueVersion), uint64(start), uint64(len(rows)), candidate})
		if marshalErr != nil {
			return InteractionResult{}, marshalErr
		}
		if len(candidateData) > announcementDataLimit {
			break
		}
		page = candidate
		data = candidateData
	}
	if len(page) == 0 {
		if start != len(rows) {
			// Put bounds text so a valid stored item always fits by itself.
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		data, err = cborx.Marshal([]any{uint64(announcementValueVersion), uint64(start), uint64(len(rows)), page})
		if err != nil {
			return InteractionResult{}, err
		}
	}
	return InteractionResult{Status: 0, Revision: &revision, Data: data}, nil
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (h AnnouncementInteractions) put(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	fingerprint, valid := announcementMutationFingerprint(request, h.CommunityID, protocol.OpAnnouncementsPut)
	if !valid {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if len(request.Args) != 2 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	payload, ok := request.Args[1].([]byte)
	if !ok || len(payload) == 0 || len(payload) > 2200 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	value, err := decodeAnnouncementValue(payload)
	if err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	argID, hasID := request.Args[0].(id.ID)
	if request.Args[0] == nil {
		if value.ID.IsZero() {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
	} else if !hasID || argID.IsZero() || value.ID != argID {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if value.Schedule.Revision == 0 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	allowed, err := h.authorize(ctx, actor)
	if err != nil {
		return InteractionResult{}, err
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	outcome, err := h.Scheduler.PutWithReceiptAtRevision(ctx, *request.Revision, request.RequestID, fingerprint, *request.Expires, value)
	if err != nil {
		if errors.Is(err, announcements.ErrSchedulerConflict) || errors.Is(err, announcements.ErrMutationIDConflict) {
			return InteractionResult{Status: interactionStatusConflict}, nil
		}
		if errors.Is(err, announcements.ErrMutationExpired) {
			return InteractionResult{Status: interactionStatusExpired}, nil
		}
		return InteractionResult{}, err
	}
	return InteractionResult{Status: outcome.Status, Revision: outcome.Revision, Data: outcome.Data}, nil
}

func (h AnnouncementInteractions) delete(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	fingerprint, valid := announcementMutationFingerprint(request, h.CommunityID, protocol.OpAnnouncementsDelete)
	if !valid {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if len(request.Args) != 2 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	announcementID, idOK := request.Args[0].(id.ID)
	expectedRevision, revisionOK := request.Args[1].(uint64)
	if !idOK || announcementID.IsZero() || !revisionOK || expectedRevision == 0 || expectedRevision > math.MaxInt64 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	allowed, err := h.authorize(ctx, actor)
	if err != nil {
		return InteractionResult{}, err
	}
	if !allowed {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	outcome, err := h.Scheduler.DeleteWithReceiptAtRevision(ctx, *request.Revision, request.RequestID, fingerprint, *request.Expires, announcementID, expectedRevision)
	if err != nil {
		if errors.Is(err, announcements.ErrSchedulerConflict) || errors.Is(err, announcements.ErrMutationIDConflict) {
			return InteractionResult{Status: interactionStatusConflict}, nil
		}
		if errors.Is(err, announcements.ErrMutationExpired) {
			return InteractionResult{Status: interactionStatusExpired}, nil
		}
		return InteractionResult{}, err
	}
	return InteractionResult{Status: outcome.Status, Revision: outcome.Revision, Data: outcome.Data}, nil
}

func announcementMutationFingerprint(request protocol.Request, community id.ID, operation protocol.Operation) ([32]byte, bool) {
	if request.RequestID.IsZero() || request.CommunityID != community || community.IsZero() || request.Operation != operation || request.Revision == nil || request.Expires == nil {
		return [32]byte{}, false
	}
	canonical, err := protocol.EncodeRequest(request)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(canonical)), true
}

// The versioned payload is [1, id, destination, text, kind, timezone, date,
// hour, minute, weekday, enabled, schedule_revision]. It remains transient in
// this handler; persistence is delegated to the encrypted native scheduler.
func decodeAnnouncementValue(raw []byte) (announcements.Announcement, error) {
	var fields []any
	if cborx.ExpectMajor(raw, cborx.MajorArray) != nil {
		return announcements.Announcement{}, errors.New("invalid announcement array")
	}
	if err := cborx.Unmarshal(raw, &fields); err != nil {
		return announcements.Announcement{}, err
	}
	if len(fields) != 12 {
		return announcements.Announcement{}, errors.New("invalid announcement value length")
	}
	version, ok := fields[0].(uint64)
	if !ok || version != announcementValueVersion {
		return announcements.Announcement{}, errors.New("invalid announcement version")
	}
	announcementID, okID := announcementPayloadID(fields[1])
	destinationID, okDest := announcementPayloadID(fields[2])
	text, okText := fields[3].(string)
	kind, okKind := fields[4].(string)
	timezone, okTimezone := fields[5].(string)
	date, okDate := fields[6].(string)
	hour, okHour := fields[7].(uint64)
	minute, okMinute := fields[8].(uint64)
	weekday, okWeekday := fields[9].(uint64)
	enabledValue, okEnabled := fields[10].(uint64)
	revision, okRevision := fields[11].(uint64)
	if !okID || !okDest || !okText || !okKind || !okTimezone || !okDate || !okHour || !okMinute || !okWeekday || !okEnabled || !okRevision || announcementID.IsZero() || destinationID.IsZero() || len(text) == 0 || len(text) > maxAnnouncementInteractionText || !utf8.ValidString(text) || strings.TrimSpace(text) == "" || len(timezone) > 128 || len(date) > 10 || hour > 23 || minute > 59 || weekday > uint64(time.Saturday) || revision == 0 || revision > math.MaxInt64 {
		return announcements.Announcement{}, errors.New("invalid announcement fields")
	}
	for _, r := range text {
		if r == 0 || r == 127 || r < 32 && r != '\n' && r != '\t' {
			return announcements.Announcement{}, errors.New("invalid announcement text")
		}
	}
	schedule := announcements.Schedule{Kind: announcements.ScheduleKind(kind), Timezone: timezone, Date: date, Hour: int(hour), Minute: int(minute), Weekday: time.Weekday(weekday), Revision: revision}
	// Validate the schedule before handing it to Scheduler.Put. A future one-off
	// date may have no occurrence left; Scheduler handles that case itself.
	if _, err := announcements.NextOccurrence(schedule, time.Now().UTC()); err != nil && !errors.Is(err, announcements.ErrNoOccurrence) {
		return announcements.Announcement{}, err
	}
	return announcements.Announcement{ID: announcementID, DestinationID: destinationID, Text: text, Schedule: schedule, Enabled: enabledValue == 1}, nil
}

func announcementSummary(value announcements.Announcement) []any {
	s := value.Schedule
	return []any{value.ID, value.DestinationID, value.Text, string(s.Kind), s.Timezone, s.Date, nonnegativeUint64(s.Hour), nonnegativeUint64(s.Minute), nonnegativeUint64(int(s.Weekday)), boolUint(value.Enabled), s.Revision}
}

func nonnegativeUint64(value int) uint64 {
	if value < 0 {
		return 0
	}
	return uint64(value)
}

func announcementPayloadID(value any) (id.ID, bool) {
	var out id.ID
	b, ok := value.([]byte)
	if !ok || len(b) != len(out) {
		return out, false
	}
	copy(out[:], b)
	return out, true
}
