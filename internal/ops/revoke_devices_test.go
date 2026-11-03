package ops_test

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/ops"
)

// TestRevokeAllDevicesRevokesEveryDeviceAndSession: every active device of the person is revoked
// and every session of the person is deleted, in one call, with one audit row.
func TestRevokeAllDevicesRevokesEveryDeviceAndSession(t *testing.T) {
	h := newOpsHarness(t)
	uid, devices := seedFactorUser(t, h)
	_, otherDevices := seedFactorUserNamed(t, h, "other")
	now := h.Clock.Now().Unix()

	res, err := ops.RevokeAllDevices(t.Context(), h.Repo, uid, now)
	if err != nil {
		t.Fatalf("RevokeAllDevices: %v", err)
	}
	if len(res.Revoked) != len(devices) || res.Sessions != int64(len(devices)) {
		t.Fatalf("result = %d devices and %d sessions, want %d and %d", len(res.Revoked), res.Sessions, len(devices), len(devices))
	}
	for _, d := range devices {
		row, err := h.Repo.GetDevice(t.Context(), d)
		if err != nil || row.RevokedAt == nil {
			t.Errorf("device %s was not revoked: %+v %v", d, row, err)
		}
		if n, _ := h.Repo.CountSessionsByDevice(t.Context(), d); n != 0 {
			t.Errorf("device %s keeps %d sessions, want 0", d, n)
		}
	}
	for _, d := range otherDevices {
		if row, _ := h.Repo.GetDevice(t.Context(), d); row.RevokedAt != nil {
			t.Errorf("another person's device %s was revoked", d)
		}
		if n, _ := h.Repo.CountSessionsByDevice(t.Context(), d); n != 1 {
			t.Errorf("another person's device %s keeps %d sessions, want 1", d, n)
		}
	}
	audit, err := h.Repo.ListAudit(t.Context(), 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	found := 0
	for _, a := range audit {
		if a.Action == "device.revoke_all" && a.Target == uid.String() {
			found++
		}
	}
	if found != 1 {
		t.Errorf("%d device.revoke_all audit rows for the person, want 1", found)
	}
}

// TestRevokeAllDevicesKeepsAnEarlierRevokeTime: a device revoked before does not move its
// revoked_at, and is not counted again.
func TestRevokeAllDevicesKeepsAnEarlierRevokeTime(t *testing.T) {
	h := newOpsHarness(t)
	uid, devices := seedFactorUser(t, h)
	first := h.Clock.Now().Unix()
	if err := h.Repo.RevokeDevice(t.Context(), devices[0], first); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	h.Clock.Advance(time.Hour)
	res, err := ops.RevokeAllDevices(t.Context(), h.Repo, uid, h.Clock.Now().Unix())
	if err != nil {
		t.Fatalf("RevokeAllDevices: %v", err)
	}
	if len(res.Revoked) != 1 || res.Revoked[0] != devices[1] {
		t.Fatalf("revoked %v, want only %s", res.Revoked, devices[1])
	}
	if row, _ := h.Repo.GetDevice(t.Context(), devices[0]); *row.RevokedAt != first {
		t.Errorf("an earlier revoke moved revoked_at from %d to %d", first, *row.RevokedAt)
	}
}

// TestRevokeAllDevicesForAnUnknownUserChangesNothing: an id that names no user is an error.
func TestRevokeAllDevicesForAnUnknownUserChangesNothing(t *testing.T) {
	h := newOpsHarness(t)
	if _, err := ops.RevokeAllDevices(t.Context(), h.Repo, id.New(), 1); err == nil {
		t.Fatal("an unknown user was revoked")
	}
}
