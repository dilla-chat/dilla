package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// maxSessionBody bounds the five-element establish array, including registration.
// Its embedded credential is separately capped at 2048 bytes.
const maxSessionBody = 8192

// classChallenge and classEstablish are the buckets the two unauthenticated
// session routes meter themselves on. Neither is one of config's §5.3 knobs:
// the two keys below are derived per request rather than configured, so the
// classes are stated here with their rate and burst rather than read out of
// config.Rate.
const (
	classChallenge = "challenge"
	classEstablish = "establish"
)

// challengeClass and establishClass are those buckets' rates. One request a
// second with a burst of ten is far above what a real client needs — a device
// asks for a nonce and spends it when it establishes or renews a session — and
// far below what enumerating a 16-byte identifier space would take. The two are
// separate classes, and therefore separate buckets, because a client that
// legitimately mints a nonce must still be able to spend it: charging both legs
// of one ceremony to one bucket would halve the real burst.
var (
	challengeClass = server.Class{Name: classChallenge, PerSecond: 1, Burst: 10}
	establishClass = server.Class{Name: classEstablish, PerSecond: 1, Burst: 10}
)

// meterSession spends one token from each of the two keys protocol/02 § Device
// sessions names for the whole section — the source address AND the device_id.
// The first bucket is defeated by a distributed caller, the second by an
// attacker-chosen id, and only both together are worth anything.
//
// It fails CLOSED, for the same reason d.meter does: a nil limiter on an
// unauthenticated route is an unmetered unauthenticated route, which is
// precisely what these buckets exist to prevent. server.RateLimiter.Allow
// prefixes the class name itself, so "addr\x00…" under one class and under the
// other are two distinct keys.
func (d Deps) meterSession(r *http.Request, class server.Class, deviceID id.ID) error {
	if d.Limiter == nil || d.Config == nil {
		return server.Errorf(server.CodeInternal, "the %s rate bucket is not wired", class.Name)
	}
	addr := server.RateKey(server.RealIP(r, d.Config.Server.TrustedProxyCIDRs))
	for _, key := range []string{"addr\x00" + addr, "device\x00" + deviceID.String()} {
		if ok, retry := d.Limiter.Allow(class, key); !ok {
			if d.Metrics != nil {
				d.Metrics.RateLimited(class.Name)
			}
			return server.RateLimitedAfter(retry)
		}
	}
	return nil
}

// SessionChallenge serves POST /v1/devices/{device_id}/sessions/challenge. It is
// unauthenticated and answers identically for an unknown device, so it is not a
// device-enumeration oracle. It is rate limited per source address AND per
// device_id: the first bucket is defeated by a distributed caller, the second by
// an attacker-chosen id, and only both together are worth anything.
func (d Deps) SessionChallenge(w http.ResponseWriter, r *http.Request) {
	deviceID, err := server.PathID(r, "device_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := d.meterSession(r, challengeClass, deviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	nonce, expires, err := d.Sessions.Challenge(r.Context(), deviceID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusCreated, []any{nonce, uint64(expires)}) //nolint:gosec // G115: a unix second or row id this server wrote, never negative
}

// SessionEstablish serves POST /v1/devices/{device_id}/sessions. The body is the
// five-element array of protocol/02 § Device sessions item 2:
//
//	[nonce(bstr 32), purpose(uint), sig(bstr 64), registration(array)|credential(bstr)|null, login(bstr|null)]
//
// and the answer is that section's seven-element array. `purpose` is bound into
// the signature preimage, so a renew signature cannot be replayed as an
// establish; `login`, when present, is the enrolment assertion, which
// auth.Sessions spends and which yields the `pending` scope. Element 3 is a
// registration only when login is present; otherwise it is ignored.
//
// It is metered on its own two keys BEFORE the body is decoded: protocol/02
// § Device sessions puts `429 E_RATE_LIMITED per source address and per
// device_id` on the whole section, and this is the unauthenticated route that
// produces the refusals that line sits next to.
func (d Deps) SessionEstablish(w http.ResponseWriter, r *http.Request) {
	deviceID, err := server.PathID(r, "device_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if err := d.meterSession(r, establishClass, deviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	var body []any
	if err := server.DecodeBody(w, r, maxSessionBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	if len(body) != 5 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"session body has %d elements, want 5", len(body)))
		return
	}
	nonce, _ := body[0].([]byte)
	purpose, ok := body[1].(uint64)
	if !ok || purpose > uint64(auth.PurposeProvisional) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "purpose must be 0, 1 or 2"))
		return
	}
	sig, _ := body[2].([]byte)
	login, _ := body[4].([]byte)
	var credential []byte
	var registration *auth.DeviceRegistration
	if len(login) > 0 {
		array, ok := body[3].([]any)
		if !ok {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "registration array required"))
			return
		}
		raw, err := cborx.Marshal(array)
		if err != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "registration array is malformed"))
			return
		}
		var device deviceRequest
		if err := cborx.Unmarshal(raw, &device); err != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "registration array is malformed"))
			return
		}
		if err := device.validate(); err != nil {
			server.WriteError(w, err)
			return
		}
		if len(device.Credential) > 2048 {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "credential is at most 2048 bytes on establish"))
			return
		}
		registration = &auth.DeviceRegistration{DeviceID: device.DeviceID, DSKPub: device.DSKPub,
			Tier: device.Tier, SignerTier: device.SignerTier, Credential: device.Credential}
	} else {
		credential, _ = body[3].([]byte)
	}

	req := auth.EstablishRequest{
		DeviceID: deviceID, Nonce: nonce, Purpose: auth.Purpose(purpose),
		Sig: sig, Credential: credential, Login: login, Registration: registration,
	}
	var tok auth.Token
	if auth.Purpose(purpose) == auth.PurposeRenew {
		tok, err = d.Sessions.Renew(r.Context(), req)
	} else {
		tok, err = d.Sessions.Establish(r.Context(), req)
	}
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	out := []any{
		tok.Token, uint64(tok.Scope), tok.UserID, tok.DeviceID,
		uint64(tok.Expires), uint64(tok.IdleExpires), tok.Generation, //nolint:gosec // G115: a unix second or row id this server wrote, never negative
	}
	d.write(w, r, http.StatusCreated, out)
}

// SessionDelete serves DELETE /v1/devices/{device_id}/sessions: it drops every
// session of one device. A device may only do this to itself or to another
// device of the same user.
//
// It goes through auth.Sessions rather than store.Repository directly, so the
// OnRevoke fan-out the composition root wires to Deps.CloseGateway runs here
// too: "log this device out" that removed the HTTP credential and left the
// socket already authenticated with it would be a logout in name only
// (protocol/02 § Device sessions item 6).
func (d Deps) SessionDelete(w http.ResponseWriter, r *http.Request) {
	sess, ok := auth.FromContext(r.Context())
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	deviceID, err := server.PathID(r, "device_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	device, err := d.Repo.GetDevice(r.Context(), deviceID)
	if err != nil {
		// storeError, not a flat 404: a store outage or a cancelled context on
		// this route must be logged and answered E_INTERNAL, not reported to the
		// client as "no such device".
		server.WriteError(w, d.storeError(r, err))
		return
	}
	// A device of another user answers 404, not 403, exactly as
	// DELETE /v1/devices/{device_id} does: whether an identifier exists is not
	// something one account tells another, and two neighbouring routes must not
	// state opposite rules about the same pair of facts. The brief's snippet
	// said 403 (recorded as a deviation).
	if device.UserID != sess.UserID {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "not found"))
		return
	}
	// The same ten-minute grace as the key-less DELETE /v1/devices/{device_id} (fix-wave review
	// NEW-1): without it a stolen enrolled session polling GET /v1/devices cut a recovering owner's
	// pending session the moment its row appeared, at no login cost, and so kept the owner from
	// publishing the list that revokes the thief. A listed row and the caller's own device are
	// never refused.
	if sess.DeviceID != device.ID && device.Created > d.Clock.Now().Unix()-keylessDeleteGrace {
		if d.Sessions.DeviceLists == nil {
			server.WriteError(w, server.Errorf(server.CodeInternal, "device-list lookup is not wired"))
			return
		}
		entries, listErr := d.Sessions.DeviceLists.ListedDevices(r.Context(), sess.UserID)
		if listErr != nil && !errors.Is(listErr, auth.ErrNoDeviceList) {
			server.WriteError(w, auth.ListError(listErr))
			return
		}
		if !auth.Listed(entries, device.ID, device.DSKPub) {
			server.WriteError(w, server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest,
				"the device registered less than 10 minutes ago and is too new to remove its sessions; its own session may remove them, or it expires unlisted after 24 hours")))
			return
		}
	}
	if _, err := d.Sessions.DeleteDeviceSessions(r.Context(), deviceID); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.noContent(w, r)
}

// GatewayTicket serves POST /v1/gateway/ticket for clients that cannot set an
// Authorization header on a WebSocket upgrade. The ticket store itself is
// internal/gateway's (interfaces.md §1, §6.1): a second in-memory store here
// would mint tickets the gateway has never heard of. Deps.Tickets is filled by
// part 1b task 17.
func (d Deps) GatewayTicket(w http.ResponseWriter, r *http.Request) {
	sess, ok := auth.FromContext(r.Context())
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	if d.Tickets == nil {
		server.WriteError(w, notImplemented("the gateway ticket store is part 1b's"))
		return
	}
	// The ticket redeems to the bearer token this request authenticated with:
	// the session middleware has already resolved it, so it is known good.
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	ticket, expires, err := d.Tickets.Mint(sess.DeviceID, token)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusCreated, []any{ticket, uint64(expires.Unix())}) //nolint:gosec // G115: a unix second or row id this server wrote, never negative
}
