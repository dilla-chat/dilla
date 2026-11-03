package ds

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestEligibleHumanInteractionDeviceRequiresExactActiveSignedEntry(t *testing.T) {
	user, deviceID := id.New(), id.New()
	key := make([]byte, 32)
	key[0] = 1
	device := store.DeviceRow{ID: deviceID, UserID: user, DSKPub: key, Tier: 1}
	entry := mlswasi.DeviceListEntry{DeviceID: deviceID[:], DSKPub: key, Tier: 1}
	if !EligibleHumanInteractionDevice(user, device, []mlswasi.DeviceListEntry{entry}) {
		t.Fatal("active browser device without VerifiedAt was refused despite exact signed-list entry")
	}
	cases := []struct {
		name    string
		device  store.DeviceRow
		entries []mlswasi.DeviceListEntry
	}{
		{"revoked device", func() store.DeviceRow { d := device; now := time.Now().Unix(); d.RevokedAt = &now; return d }(), []mlswasi.DeviceListEntry{entry}},
		{"quarantined device", func() store.DeviceRow { d := device; now := time.Now().Unix(); d.QuarantinedAt = &now; return d }(), []mlswasi.DeviceListEntry{entry}},
		{"device absent from signed list", device, nil},
		{"wrong signed key", device, []mlswasi.DeviceListEntry{{DeviceID: deviceID[:], DSKPub: make([]byte, 32), Tier: 1}}},
		{"wrong signed tier", device, []mlswasi.DeviceListEntry{{DeviceID: deviceID[:], DSKPub: key, Tier: 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if EligibleHumanInteractionDevice(user, tc.device, tc.entries) {
				t.Fatal("ineligible device accepted")
			}
		})
	}
}

func TestEligibleHumanInteractionDeviceAcceptsNativeTier(t *testing.T) {
	user, deviceID := id.New(), id.New()
	key := make([]byte, 32)
	key[0] = 2
	device := store.DeviceRow{ID: deviceID, UserID: user, DSKPub: key, Tier: 0}
	entry := mlswasi.DeviceListEntry{DeviceID: deviceID[:], DSKPub: key, Tier: 0}
	if !EligibleHumanInteractionDevice(user, device, []mlswasi.DeviceListEntry{entry}) {
		t.Fatal("active native human device without VerifiedAt was refused despite exact signed-list entry")
	}
}
