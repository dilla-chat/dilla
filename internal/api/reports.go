package api

import (
	"context"
	"crypto/hmac"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// The stable verification results a report records (protocol/09 § Reports).
// A report that does not verify is FILED, never refused: it is evidence about
// the reporter, and discarding it would hide that.
const (
	// ReportVerified: both of protocol/04's equations hold.
	ReportVerified = "verified"
	// ReportEnvelopeMalformed: the submitted bytes are not a 9-element
	// deterministic-CBOR envelope, so C cannot be computed.
	ReportEnvelopeMalformed = "envelope_malformed"
	// ReportNoCommitmentStored: the instance holds no C for the message (a row
	// written before it recorded one), so the first equation cannot be checked.
	ReportNoCommitmentStored = "no_commitment_stored"
	// ReportCommitmentMismatch: C over the submitted envelope and k_f is not
	// the C the instance stored: the envelope or k_f is not what was sent.
	ReportCommitmentMismatch = "commitment_mismatch"
	// ReportFrankingKeyUnknown: the message predates the recorded key id and
	// none of the retained keys made its tag.
	ReportFrankingKeyUnknown = "franking_key_unknown"
	// ReportFrankingKeyUnavailable: the message names a key the instance no
	// longer holds.
	ReportFrankingKeyUnavailable = "franking_key_unavailable"
	// ReportTagMismatch: C matches, but T recomputed from the stored tuple is
	// not the stored tag.
	ReportTagMismatch = "tag_mismatch"
)

// Report statuses: the moderation state an instance admin moves a report
// through with PATCH /v1/reports/{id}.
const (
	ReportOpen      uint64 = 0
	ReportResolved  uint64 = 1
	ReportDismissed uint64 = 2
)

const (
	// maxReportResultBytes bounds PATCH's result text.
	maxReportResultBytes = 1024
	// defaultReportLimit and maxReportLimit bound one GET /v1/reports page.
	defaultReportLimit = 100
	maxReportLimit     = 1000
	// reportRequestElements is POST's [group_id, seq, envelope, k_f].
	reportRequestElements = 4
)

// reportAuditAction is the audit action PATCH writes for each status.
var reportAuditAction = map[uint64]string{
	ReportOpen:      "report.reopen",
	ReportResolved:  "report.resolve",
	ReportDismissed: "report.dismiss",
}

// Reports serves protocol/09 § Reports: filing a franked report, the instance
// admin's queue, and moving a report's status. Register mounts the handlers
// bare, as Readable does, and each checks its own session.
type Reports struct {
	repo store.Repository
	keys FrankingKeys
	clk  clock.Clock
	log  *slog.Logger
	// maxCiphertext is the instance's max_ciphertext_bytes; zero means the default. A report
	// carries a whole envelope, so a legal message is never unreportable for its size.
	maxCiphertext int64
}

// WithMaxCiphertextBytes sets the instance's max_ciphertext_bytes and returns h.
func (h *Reports) WithMaxCiphertextBytes(n int64) *Reports {
	h.maxCiphertext = n
	return h
}

// NewReports wires the routes over the instance's franking key history: the
// current key and every retained one, which is what lets a report verify
// after a rotation.
func NewReports(repo store.Repository, keys FrankingKeys, clk clock.Clock, log *slog.Logger) *Reports {
	return &Reports{repo: repo, keys: keys, clk: clk, log: log}
}

func (h *Reports) Register(mux *server.Mux) {
	mux.HandleFunc("POST /v1/reports", h.create)
	mux.HandleFunc("GET /v1/reports", h.list)
	mux.HandleFunc("PATCH /v1/reports/{id}", h.patch)
}

// frankedTuple is protocol/04's stored tuple, read from whichever table holds
// the reported message. verify takes the tuple, not the table.
type frankedTuple struct {
	GroupID     id.ID // channel_id for a readable message
	Epoch       uint64
	Seq         uint64
	Uploader    id.ID
	CommitmentC []byte // nil when the instance stored none
	Tag         []byte
	KeyID       id.ID // all zero: franked before the key id was recorded
	RecvTS      int64
}

// verify runs both equations and returns one of the stable result strings.
// C is recomputed from the submitted envelope and k_f and compared with the C
// the instance stored at upload; T is recomputed from the instance's OWN
// stored tuple, over that recomputed C, under the key the message names.
func (h *Reports) verify(msg frankedTuple, envelope, kf []byte) string {
	c, err := Commitment(envelope, kf)
	if err != nil {
		return ReportEnvelopeMalformed
	}
	if msg.CommitmentC == nil {
		return ReportNoCommitmentStored
	}
	if !hmac.Equal(c, msg.CommitmentC) {
		return ReportCommitmentMismatch
	}
	// The all-zero key id means "franked before the column existed" (P2-D21),
	// so the key is unknown rather than unavailable: try every retained key,
	// current first. Without this every message older than the migration would
	// report unverifiable.
	if msg.KeyID == (id.ID{}) {
		for _, k := range h.keys.All() {
			if hmac.Equal(Tag(k.Key, msg.GroupID, msg.Epoch, msg.Seq, msg.Uploader, c, msg.RecvTS), msg.Tag) {
				return ReportVerified
			}
		}
		return ReportFrankingKeyUnknown
	}
	key, ok := h.keys.ByID(msg.KeyID)
	if !ok {
		return ReportFrankingKeyUnavailable
	}
	if !hmac.Equal(Tag(key, msg.GroupID, msg.Epoch, msg.Seq, msg.Uploader, c, msg.RecvTS), msg.Tag) {
		return ReportTagMismatch
	}
	return ReportVerified
}

// target resolves (group_id, seq) to its franking tuple in two steps. A
// group_id that names an mls_groups row reads mls_app_messages; any other is
// taken as a server-readable channel's id and reads readable_messages, whose
// tuple has channel_id in the group_id slot and epoch 0 (protocol/09 §
// Readable channels). A tombstoned or deleted message still resolves: the
// tuple outlives the content, which is the point of storing it.
func (h *Reports) target(ctx context.Context, groupID id.ID, seq uint64) (frankedTuple, error) {
	if seq > math.MaxInt64 {
		return frankedTuple{}, server.Errorf(server.CodeNotFound, "no such message")
	}
	_, err := h.repo.GetGroup(ctx, groupID)
	switch {
	case err == nil:
		msg, err := h.repo.GetAppMessage(ctx, groupID, seq)
		if err != nil {
			return frankedTuple{}, notFound(err)
		}
		return frankedTuple{
			GroupID: groupID, Epoch: msg.Epoch, Seq: msg.Seq, Uploader: msg.UploaderDevice,
			CommitmentC: msg.CommitmentC, Tag: msg.FrankingTag, KeyID: msg.FrankingKeyID, RecvTS: msg.Created,
		}, nil
	case !errors.Is(err, store.ErrNotFound):
		return frankedTuple{}, err
	}
	rows, err := h.repo.ListReadableMessages(ctx, groupID, seq, 1)
	if err != nil {
		return frankedTuple{}, err
	}
	if len(rows) == 0 || rows[0].Seq != seq {
		return frankedTuple{}, server.Errorf(server.CodeNotFound, "no such message")
	}
	m := rows[0]
	// An edit re-franks at the edit's time, which the row records as edited
	// (task 9), so that is the recv_ts the current tag was made over.
	recv := m.Created
	if m.Edited != nil {
		recv = *m.Edited
	}
	return frankedTuple{
		GroupID: groupID, Epoch: 0, Seq: m.Seq, Uploader: m.UploaderDevice,
		CommitmentC: m.CommitmentC, Tag: m.FrankingTag, KeyID: m.FrankingKeyID, RecvTS: recv,
	}, nil
}

// reportRequest is POST /v1/reports' [group_id(bstr16), seq(uint),
// envelope(bstr), k_f(bstr32)]. Each element's CBOR major type is checked
// before it is decoded: fxamacker would otherwise fill a []byte from an array.
type reportRequest struct {
	GroupID  id.ID
	Seq      uint64
	Envelope []byte
	KF       []byte
}

func decodeReportRequest(w http.ResponseWriter, r *http.Request, bodyCap int64) (reportRequest, error) {
	var raw []cbor.RawMessage
	if err := server.DecodeBody(w, r, bodyCap, &raw); err != nil {
		return reportRequest{}, err
	}
	if len(raw) != reportRequestElements {
		return reportRequest{}, server.Errorf(server.CodeInvalidRequest,
			"a report is [group_id, seq, envelope, k_f], got %d elements", len(raw))
	}
	var req reportRequest
	if err := req.GroupID.UnmarshalCBOR(raw[0]); err != nil {
		return reportRequest{}, server.Errorf(server.CodeInvalidRequest, "group_id must be 16 bytes")
	}
	var err error
	if req.Seq, err = decodeUint(raw[1]); err != nil {
		return reportRequest{}, server.Errorf(server.CodeInvalidRequest, "seq must be a uint")
	}
	if req.Envelope, err = decodeBytes(raw[2]); err != nil {
		return reportRequest{}, server.Errorf(server.CodeInvalidRequest, "envelope must be a byte string")
	}
	if req.KF, err = decodeBytes(raw[3]); err != nil || len(req.KF) != frankingKeyBytes {
		return reportRequest{}, server.Errorf(server.CodeInvalidRequest, "k_f must be %d bytes", frankingKeyBytes)
	}
	return req, nil
}

type reportCreated struct {
	_                  struct{} `cbor:",toarray"`
	ReportID           id.ID
	VerificationResult string
}

// create is POST /v1/reports -> 201 [report_id, verification_result]. Any
// enrolled session may file one. The stored revealed_envelope is exactly the
// submitted bytes, whatever the verification says, and franking_key_id is the
// message's own, not the current key.
func (h *Reports) create(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		h.fail(w, r, "file report", err)
		return
	}
	req, err := decodeReportRequest(w, r, readableBodyCap(h.maxCiphertext))
	if err != nil {
		h.fail(w, r, "file report", err)
		return
	}
	msg, err := h.target(r.Context(), req.GroupID, req.Seq)
	if err != nil {
		h.fail(w, r, "file report", err)
		return
	}
	row := store.ReportRow{
		ID: id.New(), Reporter: s.UserID, GroupID: req.GroupID, Seq: req.Seq,
		RevealedEnvelope: req.Envelope, KF: req.KF, FrankingKeyID: msg.KeyID,
		VerificationResult: h.verify(msg, req.Envelope, req.KF),
		Status:             int32(ReportOpen),
		Created:            h.clk.Now().Unix(),
	}
	if err := h.repo.PutReport(r.Context(), row); err != nil {
		h.fail(w, r, "file report", err)
		return
	}
	if err := server.EncodeBody(w, http.StatusCreated, reportCreated{
		ReportID: row.ID, VerificationResult: row.VerificationResult,
	}); err != nil {
		h.log.Error("encode report", "err", err)
	}
}

// reportItem is one row of GET /v1/reports: exactly eight elements, and
// nothing about the group beyond the one reported message (protocol/04: the
// report shows exactly the envelope submitted and nothing else).
type reportItem struct {
	_                  struct{} `cbor:",toarray"`
	ReportID           id.ID
	Reporter           id.ID
	GroupID            id.ID
	Seq                uint64
	RevealedEnvelope   []byte
	VerificationResult string
	Status             uint64
	Created            uint64
}

// list is GET /v1/reports?limit=: the queue, newest first, instance admins only.
func (h *Reports) list(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		h.fail(w, r, "list reports", err)
		return
	}
	if err := requireInstanceAdmin(r.Context(), h.repo, s.UserID); err != nil {
		h.fail(w, r, "list reports", err)
		return
	}
	limit := min(queryLimit(r, "limit", defaultReportLimit), maxReportLimit)
	if limit <= 0 {
		limit = defaultReportLimit
	}
	rows, err := h.repo.ListReports(r.Context(), limit)
	if err != nil {
		h.fail(w, r, "list reports", err)
		return
	}
	out := make([]reportItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, reportItem{
			ReportID: row.ID, Reporter: row.Reporter, GroupID: row.GroupID, Seq: row.Seq,
			RevealedEnvelope: row.RevealedEnvelope, VerificationResult: row.VerificationResult,
			Status: uint64(max(row.Status, 0)), Created: uint64(max(row.Created, 0)),
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		h.log.Error("encode reports", "err", err)
	}
}

// patch is PATCH /v1/reports/{id} [status(uint), result(tstr)] -> 204. It moves
// the report's moderation status and writes an audit row whose detail is the
// admin's result. The report's verification_result is NOT touched: it is the
// instance's cryptographic finding, and a moderator who could overwrite it
// could relabel a forgery "verified".
func (h *Reports) patch(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		h.fail(w, r, "patch report", err)
		return
	}
	reportID, err := server.PathID(r, "id")
	if err != nil {
		h.fail(w, r, "patch report", err)
		return
	}
	if err := requireInstanceAdmin(r.Context(), h.repo, s.UserID); err != nil {
		h.fail(w, r, "patch report", err)
		return
	}
	var req struct {
		_      struct{} `cbor:",toarray"`
		Status uint64
		Result string
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		h.fail(w, r, "patch report", err)
		return
	}
	action, ok := reportAuditAction[req.Status]
	if !ok {
		h.fail(w, r, "patch report", server.Errorf(server.CodeInvalidRequest, "status is 0, 1 or 2"))
		return
	}
	// A NUL is legal in a Go string and in a SQLite TEXT column but refused by
	// Postgres, so it is refused on both engines alike.
	if len(req.Result) > maxReportResultBytes || strings.ContainsRune(req.Result, 0) {
		h.fail(w, r, "patch report", server.Errorf(server.CodeInvalidRequest,
			"result must be at most %d bytes with no NUL", maxReportResultBytes))
		return
	}
	actor := s.UserID
	err = h.repo.Tx(r.Context(), func(tx store.Repository) error {
		row, err := tx.GetReport(r.Context(), reportID)
		if err != nil {
			return notFound(err)
		}
		if err := tx.UpdateReportStatus(r.Context(), reportID, int32(req.Status), row.VerificationResult); err != nil { //nolint:gosec // G115: 0..2, checked above
			return err
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &actor, Action: action, Target: reportID.String(), Detail: req.Result,
			At: h.clk.Now().Unix(),
		})
	})
	if err != nil {
		h.fail(w, r, "patch report", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Reports) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var se *server.Error
	if !errors.As(err, &se) {
		h.log.ErrorContext(r.Context(), what, "err", err)
	}
	server.WriteError(w, err)
}
