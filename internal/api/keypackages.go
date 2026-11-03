package api

// keypackages.go and welcomes.go are the directory half of the delivery service's HTTP surface:
// interfaces.md §5.1 rows 10 (`POST /v1/keypackages`), 11
// (`GET /v1/devices/{device_id}/keypackage`), 15 (`GET /v1/welcomes`) and 16
// (`DELETE /v1/welcomes/{welcome_id}`).
//
// Three of the four accept a PROVISIONAL session as well as an enrolled one ("E or V" in
// protocol/02's table): a device that is not yet enrolled publishes the one KeyPackage its pairing
// group's Welcome will consume, and collects and acknowledges that group's Welcome. The middleware
// admits the scope; the delivery service is what keeps such a session inside its one group, and
// answers E_PROVISIONAL_OUTSIDE_PAIRING when it reaches past it.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/server"
)

// RegisterDirectory mounts the four directory routes. It is a third method on Groups rather than
// four more lines in Register for the same reason RegisterSequencer is a second: the delivery
// service's surface grows one named group per task.
//
// `sessions.Middleware(f, auth.ScopeProvisional)` admits a provisional session OR an enrolled one
// — that is what the two scope clauses in (*auth.Sessions).Middleware mean — so it is the "E or V"
// of protocol/02's table, not "V only".
func (h *Groups) RegisterDirectory(mux *server.Mux, sessions *auth.Sessions) {
	enrolled := func(f http.HandlerFunc) http.Handler {
		return sessions.Middleware(f, auth.ScopeEnrolled)
	}
	orProvisional := func(f http.HandlerFunc) http.Handler {
		return sessions.Middleware(f, auth.ScopeProvisional)
	}
	mux.Handle("POST /v1/keypackages", orProvisional(dsMeter(h.Limiter, dsClassWrite, h.publishKeyPackages)))
	mux.Handle("GET /v1/devices/{device_id}/keypackage", enrolled(dsMeter(h.Limiter, dsClassRead, h.takeKeyPackage)))
	mux.Handle("GET /v1/welcomes", orProvisional(dsMeter(h.Limiter, dsClassRead, h.welcomes)))
	mux.Handle("DELETE /v1/welcomes/{welcome_id}", orProvisional(dsMeter(h.Limiter, dsClassWrite, h.ackWelcome)))
}

type publishRequest struct {
	_          struct{} `cbor:",toarray"`
	Packages   [][]byte
	LastResort []byte
}

type publishResponse struct {
	_     struct{} `cbor:",toarray"`
	Count uint64
}

// publishKeyPackages is row 10: `[packages([bstr]), last_resort(bstr|null)]` -> `201 [count]`.
func (h *Groups) publishKeyPackages(w http.ResponseWriter, r *http.Request) {
	var body publishRequest
	// The delivery-service body cap, not §5.3's 64 KiB: a full refill is `max_keypackages_per_device`
	// KeyPackages in one request.
	if err := server.DecodeBody(w, r, h.max(), &body); err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	n, err := h.DS.PublishKeyPackages(r.Context(), session, body.Packages, body.LastResort)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	if err := server.EncodeBody(w, http.StatusCreated, publishResponse{Count: uint64(n)}); err != nil { //nolint:gosec // G115: a count of stored rows, never negative
		server.WriteError(w, err)
		return
	}
	// Only an enrolled session's publish is handed on: a provisional (pairing) device's one
	// KeyPackage belongs to its pairing group's Welcome, and a device no signed list names yet is
	// not one the DMs may Add (invariant 4; the delivery service refuses it in any case).
	if h.AfterKeyPackages != nil && session.Scope == auth.ScopeEnrolled {
		user, device := session.UserID, session.DeviceID
		h.runAfter(w, r, func(ctx context.Context) { h.AfterKeyPackages(ctx, user, device) })
	}
}

type keyPackageResponse struct {
	_          struct{} `cbor:",toarray"`
	Blob       []byte
	LastResort uint8
	KPRef      []byte
}

// takeKeyPackage is row 11: one package for the named device. `last_resort` is a uint on the wire,
// not a bool — protocol/02's table says `last_resort(uint)` — and it is what tells a caller that
// the directory is empty and this package will be served again.
func (h *Groups) takeKeyPackage(w http.ResponseWriter, r *http.Request) {
	deviceID, err := server.PathID(r, "device_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	session, err := sessionOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// The (requester, target) bucket: each fetch consumes one of the target's KeyPackages.
	if h.Limiter != nil {
		if err := allowDS(h.Limiter, dsClassKeyPackage, keyPackageBucket(session.DeviceID, deviceID)); err != nil {
			server.WriteError(w, err)
			return
		}
	}
	kp, err := h.DS.TakeKeyPackage(r.Context(), session, deviceID)
	if err != nil {
		server.WriteError(w, dsError(err))
		return
	}
	var last uint8
	if kp.LastResort {
		last = 1
	}
	if err := server.EncodeBody(w, http.StatusOK, keyPackageResponse{
		Blob: kp.Blob, LastResort: last, KPRef: kp.KPRef,
	}); err != nil {
		server.WriteError(w, err)
	}
}

// parseWelcomeID reads the `{welcome_id}` path segment. It is a decimal int64, not an id.ID: the
// surrogate key of `mls_welcomes` is the table's AUTOINCREMENT column, the one identifier in the
// protocol that is not 16 random bytes.
func parseWelcomeID(r *http.Request) (int64, error) {
	v, err := strconv.ParseInt(r.PathValue("welcome_id"), 10, 64)
	if err != nil || v <= 0 {
		return 0, server.Errorf(server.CodeInvalidRequest, "welcome_id must be a positive decimal integer")
	}
	return v, nil
}
