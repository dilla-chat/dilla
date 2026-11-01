package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/jonasthim/dilla/internal/auth"
	botaccess "github.com/jonasthim/dilla/internal/bot/access"
	"github.com/jonasthim/dilla/internal/bot/identity"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// createDeviceRequest is POST /v1/devices' body: the device array of POST /v1/accounts followed by
// the proof of possession of dsk_pub, [device_id, dsk_pub, tier, signer_tier, credential,
// nonce(32), sig(64)], where nonce comes from POST /v1/devices/{device_id}/sessions/challenge for
// the new device_id and sig is the establish signature by the new DSK over it (purpose 0).
type createDeviceRequest struct {
	_          struct{} `cbor:",toarray"`
	DeviceID   id.ID
	DSKPub     []byte
	Tier       uint8
	SignerTier uint8
	Credential []byte
	Nonce      []byte
	Sig        []byte
}

// CreateDevice is POST /v1/devices: [device_id, dsk_pub, tier, signer_tier,
// credential, nonce, sig] → [device_id]. The device is always the session's own
// user's; a body cannot name another user. The caller proves it holds dsk_pub's
// private half exactly as establish does (security review F2), and no other live
// row of the user may hold the same key (409). Admission uses the same live-row
// cap, expiry and live-row hourly rate as assertion registration, but past the cap
// it answers 403 "device cap reached" and past the rate 429 with retry_after_ms, and
// it never evicts (branch review REGISTRATION-DEVICES-03): its caller pays no host
// login, so eviction here would let a stolen enrolled session replace a recovering
// owner's new row at will.
func (d Deps) CreateDevice(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	var body createDeviceRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	req := deviceRequest{DeviceID: body.DeviceID, DSKPub: body.DSKPub, Tier: body.Tier,
		SignerTier: body.SignerTier, Credential: body.Credential}
	if err := req.validate(); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := d.Sessions.ProveDeviceKey(req.DeviceID, req.DSKPub, body.Nonce, body.Sig); err != nil {
		server.WriteError(w, err)
		return
	}
	// Task 8b security review M1 (fail closed): the credential is decoded and verified by the same WASI core every other
	// credential boundary uses (canonical CBOR, the UMK→SSK and SSK→device signatures over this dsk_pub). It must name
	// this device, its tiers, and the account's own kind — a member cannot register a bot-kind credential and speak as
	// a Bot to clients that trust a bot-kind sender. Its user id is the session's, or zero (the unbound human form
	// signup issues and the core uses until the server assigns the id). Anything that does not verify is refused.
	if d.BotIdentity == nil {
		server.WriteError(w, server.Errorf(server.CodeInternal, "credential verifier is not wired"))
		return
	}
	credential, verifyErr := d.BotIdentity.Verify(r.Context(), req.Credential, req.DSKPub)
	if verifyErr != nil {
		if errors.Is(verifyErr, identity.ErrUnavailable) {
			server.WriteError(w, server.Errorf(server.CodeInternal, "credential verifier unavailable"))
		} else {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "device credential is invalid"))
		}
		return
	}
	account, accountErr := d.Repo.GetUser(r.Context(), sess.UserID)
	if accountErr != nil {
		server.WriteError(w, d.storeError(r, accountErr))
		return
	}
	// Task S1: the credential is also bound to the account's own identity keys, as signup binds the
	// first device's (accounts.go): its UMK, its SSK and the UMK's signature over that SSK are the
	// ones users holds. A credential under any other UMK/SSK pair is a device of another identity.
	if uint64(credential.Kind) != uint64(account.Kind) || credential.DeviceID != req.DeviceID ||
		(credential.UserID != (id.ID{}) && credential.UserID != sess.UserID) ||
		credential.Tier != req.Tier || credential.SignerTier != req.SignerTier ||
		!bytes.Equal(credential.UMKPub[:], account.UMKPub) || !bytes.Equal(credential.SSKPub[:], account.SSKPub) ||
		!bytes.Equal(credential.SigUMKSSK[:], account.SigUMKSSK) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "device credential does not match the account or device"))
		return
	}
	now := d.Clock.Now().Unix()
	row := store.DeviceRow{
		ID: req.DeviceID, UserID: sess.UserID, DSKPub: req.DSKPub, Tier: req.Tier,
		SignerTier: req.SignerTier, CredentialBlob: req.Credential, LastSeen: now, Created: now,
	}
	if d.Sessions.DeviceLists == nil {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	entries, listErr := d.Sessions.DeviceLists.ListedDevices(r.Context(), sess.UserID)
	noList := errors.Is(listErr, auth.ErrNoDeviceList)
	if noList {
		entries = nil
	} else if listErr != nil {
		server.WriteError(w, auth.ListError(listErr))
		return
	}
	err := d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := d.Sessions.AdmitEnrolledDevice(r.Context(), tx, sess.UserID, entries, noList, req.DSKPub, now); err != nil {
			return err
		}
		return tx.CreateDevice(r.Context(), row)
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			server.WriteError(w, server.WithStatus(http.StatusConflict,
				server.Errorf(server.CodeInvalidRequest, "device_id already exists")))
			return
		}
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.write(w, r, http.StatusOK, []any{row.ID})
}

// keylessDeleteGrace is how old, in seconds, an unlisted row must be before a session of another
// device may remove it with the key-less DELETE (branch review REGISTRATION-DEVICES-03).
const keylessDeleteGrace = 600

// ListDevices is GET /v1/devices →
// [[device_id, tier, signer_tier, verified_at|null, revoked_at|null, last_seen, label|null]].
func (d Deps) ListDevices(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	rows, err := d.Repo.ListDevicesByUser(r.Context(), sess.UserID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, []any{
			row.ID, uint64(row.Tier), uint64(row.SignerTier),
			nullableTime(row.VerifiedAt), nullableTime(row.RevokedAt), uint64(row.LastSeen), //nolint:gosec // G115: a unix second or row id this server wrote, never negative
			row.Label,
		})
	}
	d.write(w, r, http.StatusOK, out)
}

// maxDeviceLabel is the longest device name, in characters (protocol/09 § Devices).
const maxDeviceLabel = 64

// patchDeviceRequest is PATCH /v1/devices/{device_id}'s body: [label|null]. Null clears the name.
type patchDeviceRequest struct {
	_     struct{} `cbor:",toarray"`
	Label *string
}

// normalizeDeviceLabel is the stored form of a device name: the shared text rule of
// auth.NormalizeUserText (NFC, trimmed, at most 64 characters, one line, no control, bidi,
// zero-width or other invisible character). A name that is empty once trimmed is refused;
// clearing is null.
func normalizeDeviceLabel(s string) (string, error) {
	out, err := auth.NormalizeUserText(s, maxDeviceLabel, false)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", auth.ErrTextBlank
	}
	return out, nil
}

// PatchDevice is PATCH /v1/devices/{device_id}: [label|null] → 204. It names (or, with null,
// unnames) a device of the caller's own account, this one or a sibling; the owner is part of the
// store's UPDATE, so another user's device answers 404 exactly as an unknown id does. The name is
// not in the signed device list: the server stores it readable, like a display name, and could
// change it (protocol/09).
func (d Deps) PatchDevice(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	deviceID, err := server.PathID(r, "device_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body patchDeviceRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	if body.Label != nil {
		label, err := normalizeDeviceLabel(*body.Label)
		if err != nil {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "label must be 1 to %d characters with no control, bidi or zero-width character, or null", maxDeviceLabel))
			return
		}
		body.Label = &label
	}
	if err := d.Repo.SetDeviceLabel(r.Context(), sess.UserID, deviceID, body.Label); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.noContent(w, r)
}

// DeleteDevice is DELETE /v1/devices/{device_id} → 204. protocol/02 § Device
// sessions item 6: revoking a device MUST delete its session rows and close its
// gateway connections in the same operation, which is what
// auth.Sessions.RevokeDevice does — the rows in one transaction, the sockets
// through the OnRevoke hook the composition root wires to Deps.CloseGateway.
func (d Deps) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	deviceID, err := server.PathID(r, "device_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	row, err := d.Repo.GetDevice(r.Context(), deviceID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	// A device of another user answers 404, not 403: whether an identifier
	// exists is not something one account tells another.
	if row.UserID != sess.UserID {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "not found"))
		return
	}
	if d.Sessions.DeviceLists == nil {
		server.WriteError(w, server.Errorf(server.CodeInternal, "device-list lookup is not wired"))
		return
	}
	entries, listErr := d.Sessions.DeviceLists.ListedDevices(r.Context(), sess.UserID)
	if listErr != nil && !errors.Is(listErr, auth.ErrNoDeviceList) {
		server.WriteError(w, auth.ListError(listErr))
		return
	}
	// Listed means the (device_id, dsk_pub) pair (security review F2): a row that only copies a
	// listed key is not the listed device, and the owner removes it here like any unlisted row.
	if auth.Listed(entries, row.ID, row.DSKPub) {
		server.WriteError(w, server.WithStatus(http.StatusConflict,
			server.Errorf(server.CodeInvalidRequest, "a listed device is revoked by a signed device list")))
		return
	}
	// A recovering owner's new row is unlisted until the list PUT that names it; without a grace a
	// stolen enrolled session polling GET /v1/devices removed it the moment it appeared, at no login
	// cost, and so kept the owner from publishing the list that revokes the thief. The owner's own
	// young strays are replaced by registration and expire at 24 hours, so the grace locks no one out.
	if sess.DeviceID != row.ID && row.Created > d.Clock.Now().Unix()-keylessDeleteGrace {
		server.WriteError(w, server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest,
			"the device registered less than 10 minutes ago and is too new to remove; its own session may remove it, or it expires unlisted after 24 hours")))
		return
	}
	if err := d.Sessions.RevokeDevice(r.Context(), deviceID); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	d.noContent(w, r)
}

// deviceListRequest is PUT /v1/users/{user_id}/device-list's body:
// [version, blob, ssk_signature(64), prev_hash(32)].
type deviceListRequest struct {
	_            struct{} `cbor:",toarray"`
	Version      uint64
	Blob         []byte
	SSKSignature []byte
	PrevHash     []byte
}

// PutDeviceList publishes a signed device list. The instance verifies the outer elements against
// the blob in Go, the signature and user in the guest, and the version and hash chain against the
// stored newest. It revokes named devices and deletes their sessions in the storing transaction;
// after commit it closes their sockets and asks the delivery service to remove their leaves.
func (d Deps) PutDeviceList(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	userID, err := server.PathID(r, "user_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if userID != sess.UserID {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "a device list is published by its own user"))
		return
	}
	var req deviceListRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		server.WriteError(w, err)
		return
	}
	if len(req.SSKSignature) != sigLen || len(req.PrevHash) != 32 || len(req.Blob) == 0 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest,
			"ssk_signature is 64 bytes, prev_hash is 32 and blob is not empty"))
		return
	}
	version, prevHash, sig, ok := listOuter(req.Blob)
	if !ok || version != req.Version || !bytes.Equal(prevHash, req.PrevHash) || !bytes.Equal(sig, req.SSKSignature) {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "device list elements disagree"))
		return
	}
	if d.DeviceLists == nil {
		err := server.Errorf(server.CodeInternal, "device-list verifier is not wired")
		d.logf(r, "api: device list", "err", err)
		server.WriteError(w, err)
		return
	}
	user, err := d.Repo.GetUser(r.Context(), userID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	entries, err := d.DeviceLists.Verify(r.Context(), req.Blob, user.SSKPub, userID)
	if err != nil {
		var abiErr *mlswasi.ABIError
		if errors.As(err, &abiErr) && abiErr.Code == "E_CREDENTIAL" {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "device list does not verify"))
			return
		}
		d.logf(r, "api: device list verification", "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, ""))
		return
	}
	botBinding, isBot, botErr := botaccess.ForUserDevice(r.Context(), d.Repo, userID, sess.DeviceID)
	if botErr != nil {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "managed bot device is no longer eligible"))
		return
	}
	if isBot {
		device, err := d.Repo.GetDevice(r.Context(), sess.DeviceID)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		if botBinding.InstallationID.IsZero() || userID != sess.UserID || len(entries) != 1 || entries[0].Revoked ||
			!bytes.Equal(entries[0].DeviceID, sess.DeviceID[:]) || !bytes.Equal(entries[0].DSKPub, device.DSKPub) || entries[0].Tier != tierNative {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "bot device list must contain exactly its own native device"))
			return
		}
	}
	ctx := r.Context()
	row := store.DeviceListRow{
		UserID: userID, Version: req.Version, Blob: req.Blob,
		SSKSignature: req.SSKSignature, PrevHash: req.PrevHash, Created: d.Clock.Now().Unix(),
	}
	eventTimestamp, validEventTimestamp := checkedUint64(row.Created)
	if !isBot && !validEventTimestamp {
		server.WriteError(w, server.Errorf(server.CodeInternal, "device-list publication timestamp is unavailable"))
		return
	}
	versionConflict := func() error {
		return server.WithStatus(http.StatusConflict,
			server.Errorf(server.CodeInvalidRequest, "device-list version %d is not the next version", req.Version))
	}
	var revoked []id.ID
	err = d.Repo.Tx(ctx, func(tx store.Repository) error {
		newest, err := tx.GetDeviceList(ctx, userID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			if req.Version != 1 || !bytes.Equal(req.PrevHash, make([]byte, 32)) {
				return versionConflict()
			}
		case err != nil:
			return err
		default:
			sum := sha256.Sum256(newest.Blob)
			if req.Version != newest.Version+1 || !bytes.Equal(req.PrevHash, sum[:]) {
				return versionConflict()
			}
		}
		if err := tx.PutDeviceList(ctx, row); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return versionConflict()
			}
			return err
		}
		if !isBot {
			communities, err := tx.ListCommunitiesForUser(ctx, userID)
			if err != nil {
				return err
			}
			for _, community := range communities {
				eventID := deviceListEventID(community.ID, userID, req.Version)
				if _, err := tx.AppendBotStructuralEvent(ctx, protocol.Event{ID: eventID, CommunityID: community.ID,
					Timestamp: eventTimestamp, Kind: protocol.EventDeviceList, Data: []any{userID, req.Version}}); err != nil {
					return err
				}
			}
		}
		for _, entry := range entries {
			if !entry.Revoked || len(entry.DeviceID) != 16 {
				continue
			}
			var dev id.ID
			copy(dev[:], entry.DeviceID)
			device, err := tx.GetDevice(ctx, dev)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if device.UserID != userID || device.RevokedAt != nil {
				continue
			}
			if err := tx.RevokeDevice(ctx, dev, row.Created); err != nil {
				return err
			}
			if _, err := tx.DeleteSessionsByDevice(ctx, dev); err != nil {
				return err
			}
			revoked = append(revoked, dev)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			server.WriteError(w, versionConflict())
			return
		}
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if d.Sessions != nil && d.Sessions.OnRevoke != nil {
		for _, dev := range revoked {
			d.Sessions.OnRevoke(dev)
		}
	}
	d.noContent(w, r)
	if d.AfterDeviceList != nil {
		_ = http.NewResponseController(w).Flush()
		d.AfterDeviceList(context.WithoutCancel(r.Context()), userID, revoked)
	}
}

func deviceListEventID(community, user id.ID, version uint64) id.ID {
	h := sha256.New()
	_, _ = h.Write([]byte("dilla bot device-list event v1\x00"))
	_, _ = h.Write(community[:])
	_, _ = h.Write(user[:])
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], version)
	_, _ = h.Write(v[:])
	sum := h.Sum(nil)
	var out id.ID
	copy(out[:], sum[:len(out)])
	out[6] = (out[6] & 0x0f) | 0x50
	out[8] = (out[8] & 0x3f) | 0x80
	return out
}

// GetDeviceList permits enrolled sessions to read any user and pending sessions only their own.
func (d Deps) GetDeviceList(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	userID, err := server.PathID(r, "user_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if sess.Scope == auth.ScopePending && sess.UserID != userID {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "a pending session reads only its own device list"))
		return
	}
	if r.URL.Query().Has("after") {
		after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		if err != nil || after > math.MaxInt64 {
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "after must be a decimal uint"))
			return
		}
		rows, err := d.Repo.ListDeviceListsAfter(r.Context(), userID, after, deviceListHistoryPage)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		out := make([]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, []any{row.Version, row.Blob, row.SSKSignature, row.PrevHash})
		}
		d.write(w, r, http.StatusOK, out)
		return
	}
	row, err := d.Repo.GetDeviceList(r.Context(), userID)
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	head := []any{row.Version, row.Blob, row.SSKSignature, row.PrevHash}
	// Wire 4 (Task S1 part 2): the head carries the instance-signed account-kind attestation as
	// its fifth element; a wire-2 or wire-3 session reads the four elements it always has.
	if sess.WireVersion >= accountKindWire {
		account, err := d.Repo.GetUser(r.Context(), userID)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		// A tombstoned account is served no attestation (fix round 1, kept by Ruling AW): its
		// identity is purged, and a client that verified one before keeps it.
		if account.DeletedAt != nil {
			d.write(w, r, http.StatusOK, head)
			return
		}
		attestation, err := AccountKindAttestation(d.SignAccountKind, d.Instance.InstanceID, d.Instance.ExternalSenderKeyID, account)
		if err != nil {
			d.logf(r, "api: account-kind attestation", "err", err)
			server.WriteError(w, server.Errorf(server.CodeInternal, "account-kind attestation unavailable"))
			return
		}
		head = append(head, attestation)
	}
	d.write(w, r, http.StatusOK, head)
}

// nullableTime renders a nullable unix second as a CBOR uint or null.
func nullableTime(v *int64) any {
	if v == nil {
		return nil
	}
	return uint64(*v) //nolint:gosec // G115: a unix second or row id this server wrote, never negative
}
