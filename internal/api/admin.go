package api

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"

	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/obs"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Admin serves the instance-admin routes of protocol/09 § Admin: the blob purge,
// the audit log, disabling a user and the diagnostics report. Every route
// requires an enrolled session whose user carries the instance-admin flag
// (users.flags bit 0, NV8); Register mounts the handlers bare, as Blobs does,
// and each checks both itself.
type Admin struct {
	repo    store.Repository
	store   *blob.Store
	clk     clock.Clock
	log     *slog.Logger
	metrics *obs.Metrics
	// diagnose runs the `dillad doctor` legs a running instance can answer
	// (P2-5); nil answers GET /v1/admin/diagnostics with 501.
	diagnose func(context.Context) ops.Report
	// calls cuts a disabled user's devices from every live call after the commit; nil touches none.
	calls *Calls
}

// WithCalls sets the call routes a user disable cuts the user's devices through, and returns a.
func (a *Admin) WithCalls(calls *Calls) *Admin {
	a.calls = calls
	return a
}

// NewAdmin wires the admin routes over the repository and the blob store the
// purge unlinks from.
func NewAdmin(repo store.Repository, bs *blob.Store, clk clock.Clock, log *slog.Logger) *Admin {
	return &Admin{repo: repo, store: bs, clk: clk, log: log}
}

// WithMetrics records every purge in m (dilla_blob_purges_total) and returns a.
func (a *Admin) WithMetrics(m *obs.Metrics) *Admin {
	a.metrics = m
	return a
}

// WithDiagnostics sets the report GET /v1/admin/diagnostics answers and
// returns a. The composition root passes the doctor legs that hold inside the
// serving process (internal/dillad); the ones that need a network probe stay
// `dillad doctor`'s.
func (a *Admin) WithDiagnostics(run func(context.Context) ops.Report) *Admin {
	a.diagnose = run
	return a
}

func (a *Admin) Register(mux *server.Mux) {
	mux.Socket("DELETE /v1/admin/blobs/{blob_id}", http.HandlerFunc(a.purgeBlob))
	mux.Socket("GET /v1/admin/audit", http.HandlerFunc(a.audit))
	mux.Socket("POST /v1/admin/users/{id}/disable", http.HandlerFunc(a.disableUser))
	mux.Socket("GET /v1/admin/diagnostics", http.HandlerFunc(a.diagnostics))
}

// diagnosticsLeg is one element of the diagnostics body: `[name(tstr),
// status(uint), detail(tstr), fix(tstr)]`, status 0 OK, 1 WARN, 2 FAIL — the
// data `dillad doctor` prints, in the order the legs ran (P2-5).
type diagnosticsLeg struct {
	_      struct{} `cbor:",toarray"`
	Name   string
	Status uint64
	Detail string
	Fix    string
}

// diagnostics is GET /v1/admin/diagnostics. The report runs only after the
// admin gate, so a caller who may not read it cannot make the instance run it.
func (a *Admin) diagnostics(w http.ResponseWriter, r *http.Request) {
	if _, err := a.adminSession(r); err != nil {
		server.WriteError(w, err)
		return
	}
	if a.diagnose == nil {
		server.WriteError(w, notImplemented("this instance runs no diagnostics report"))
		return
	}
	report := a.diagnose(r.Context())
	out := make([]diagnosticsLeg, 0, len(report.Legs))
	for _, l := range report.Legs {
		out = append(out, diagnosticsLeg{Name: l.Name, Status: uint64(l.Status), Detail: l.Detail, Fix: l.Fix})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		a.log.WarnContext(r.Context(), "write diagnostics", "err", err)
	}
}

// The bounds of the admin bodies.
const (
	// defaultAuditLimit and maxAuditLimit bound one GET /v1/admin/audit page.
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
)

// requireInstanceAdmin is the gate on every /v1/admin route, after
// enrolledSession: the session's user must have users.flags bit 0 set. Anyone
// else is 403 E_FORBIDDEN.
func requireInstanceAdmin(ctx context.Context, repo store.Repository, userID id.ID) error {
	u, err := repo.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return server.Errorf(server.CodeForbidden, "instance admin only")
	}
	if err != nil {
		return err
	}
	if u.Flags&store.UserFlagInstanceAdmin == 0 {
		return server.Errorf(server.CodeForbidden, "instance admin only")
	}
	return nil
}

// adminSession is enrolledSession plus requireInstanceAdmin. Handlers that
// take a path value parse it between the two, so a malformed identifier never
// reaches the database (protocol/09 § Identifiers).
func (a *Admin) adminSession(r *http.Request) (id.ID, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return id.ID{}, err
	}
	if err := requireInstanceAdmin(r.Context(), a.repo, s.UserID); err != nil {
		return id.ID{}, err
	}
	return s.UserID, nil
}

// purgeBlob is DELETE /v1/admin/blobs/{blob_id} with `[reason(tstr)]`: it
// removes every reference, deletes the blobs row, writes a tombstone and an
// audit row in one transaction, and unlinks the file immediately after the
// commit. The tombstone is not optional: without it, anyone still holding the
// ciphertext re-PUTs it and content addressing hands back the same name. A purge
// of bytes the instance does not hold still writes the tombstone, which makes
// the table the operator's hash blocklist (gap-47 §7.3).
//
// And the caveat the host guide must carry in writing: every attachment is
// encrypted under a random per-attachment key, so the same file sent by someone
// else has a different blob_id. A purge removes BYTES, not CONTENT.
func (a *Admin) purgeBlob(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	blobID, err := server.PathDigest(r, "blob_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireInstanceAdmin(r.Context(), a.repo, s.UserID); err != nil {
		server.WriteError(w, err)
		return
	}
	admin := s.UserID
	var req struct {
		_      struct{} `cbor:",toarray"`
		Reason string
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	// A NUL is legal in a Go string and in a SQLite TEXT column but refused by
	// Postgres, so it is refused on both engines alike.
	if !blob.ValidPurgeReason(req.Reason) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"reason must be 1..%d bytes with no NUL", blob.MaxPurgeReasonBytes))
		return
	}
	res, err := blob.Purge(r.Context(), a.repo, a.store, blob.PurgeRequest{
		BlobID: blobID, Reason: req.Reason, By: admin, At: a.clk.Now().Unix(),
	})
	if err != nil {
		server.WriteError(w, err)
		return
	}
	target := hex.EncodeToString(blobID)
	if res.BackupKept {
		// Every reference is gone and audited; the bytes are a user's backup object and stay
		// (BACKUPS-RECOVERY-05: this was a 409 that left every reference in place).
		a.metrics.BlobPurged()
		a.log.InfoContext(r.Context(), "blob references purged; a backup object keeps the bytes", "blob_id", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if res.UnlinkErr != nil {
		// The row and every reference are gone and the tombstone refuses the bytes
		// on every route, so a file left behind is unreachable; dillad doctor
		// reports it as an orphan.
		a.log.ErrorContext(r.Context(), "unlink purged blob", "blob_id", target, "err", res.UnlinkErr)
	}
	a.metrics.BlobPurged()
	a.log.InfoContext(r.Context(), "blob purged", "blob_id", target)
	w.WriteHeader(http.StatusNoContent)
}

// auditItem is one row of GET /v1/admin/audit.
type auditItem struct {
	_      struct{} `cbor:",toarray"`
	Actor  *id.ID
	Action string
	Target string
	Detail string
	At     uint64
}

// audit is GET /v1/admin/audit?since=&limit=: the audit log's rows at or after
// `since` (unix seconds, default 0), newest first, at most `limit` (default 100,
// at most 1000), as `[[actor|null, action, target, detail, at]]`.
func (a *Admin) audit(w http.ResponseWriter, r *http.Request) {
	if _, err := a.adminSession(r); err != nil {
		server.WriteError(w, err)
		return
	}
	since := queryCursor(r, "since")
	limit := min(queryLimit(r, "limit", defaultAuditLimit), maxAuditLimit)
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	rows, err := a.repo.ListAudit(r.Context(), since, limit)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	out := make([]auditItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, auditItem{
			Actor: row.Actor, Action: row.Action, Target: row.Target, Detail: row.Detail,
			At: uint64(max(row.At, 0)),
		})
	}
	if err := server.EncodeBody(w, http.StatusOK, out); err != nil {
		a.log.Error("encode audit", "err", err)
	}
}

// disableUser is POST /v1/admin/users/{id}/disable with `[disabled(uint)]`: 1
// sets users.disabled_at and, in the same transaction, deletes every session of
// every device of that user (protocol/02 §2.2 point 6); 0 clears disabled_at.
// Either is audited.
func (a *Admin) disableUser(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	target, err := server.PathID(r, "id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := requireInstanceAdmin(r.Context(), a.repo, s.UserID); err != nil {
		server.WriteError(w, err)
		return
	}
	admin := s.UserID
	var req struct {
		_        struct{} `cbor:",toarray"`
		Disabled uint64
	}
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if req.Disabled > 1 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "disabled is 0 or 1"))
		return
	}
	now := a.clk.Now().Unix()
	err = a.repo.Tx(r.Context(), func(tx store.Repository) error {
		if _, err := tx.GetUser(r.Context(), target); err != nil {
			return notFound(err)
		}
		action := "user.enable"
		var at *int64
		if req.Disabled == 1 {
			action, at = "user.disable", &now
		}
		if err := tx.SetUserDisabled(r.Context(), target, at); err != nil {
			return err
		}
		if at != nil {
			if _, err := tx.DeleteSessionsByUser(r.Context(), target); err != nil {
				return err
			}
		}
		return tx.Audit(r.Context(), store.AuditRow{
			Actor: &admin, Action: action, Target: target.String(), At: now,
		})
	})
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// A disabled user's devices are queued to leave every live call (the retry loop cuts them at
	// once, off this request's goroutine), not left to the room sweep.
	if req.Disabled == 1 && a.calls != nil {
		a.calls.CutUser(context.WithoutCancel(r.Context()), target)
	}
	w.WriteHeader(http.StatusNoContent)
}
