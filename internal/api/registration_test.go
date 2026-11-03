package api_test

// registration_test.go holds what an honest registration over POST /v1/groups needs since
// hardening G: the route adopts a tree of exactly one leaf, the registering device's own, keyed by
// its registered key and listed in its user's signed device list. testkit/fixtures/registration
// holds real one-leaf groups (gen-registration-groups), created by the 1,500-leaf fixture's
// creator; registrantToken makes that creator a device of this instance, as signup leaves one.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

const apiRegistrationDir = "../../testkit/fixtures/registration"

type apiRegistrationGroup struct {
	groupID     id.ID
	binding     []byte
	groupInfo   []byte
	ratchetTree []byte
}

type apiRegistration struct {
	userID, deviceID, targetID id.ID
	dsk                        []byte
	groups                     map[string]apiRegistrationGroup
}

var (
	apiRegistrationOnce sync.Once
	apiRegistrationVal  apiRegistration
	apiRegistrationErr  error
)

func apiRegistrationData(t *testing.T) apiRegistration {
	t.Helper()
	apiRegistrationOnce.Do(func() { apiRegistrationVal, apiRegistrationErr = loadAPIRegistration() })
	if apiRegistrationErr != nil {
		t.Fatalf("testkit/fixtures/registration: %v; regenerate it with\n"+
			"  cargo run -p dilla-testkit --bin dilla-testkit -- gen-registration-groups "+
			"--out testkit/fixtures/registration/", apiRegistrationErr)
	}
	return apiRegistrationVal
}

func loadAPIRegistration() (apiRegistration, error) {
	raw, err := os.ReadFile(filepath.Join(apiRegistrationDir, "manifest.json"))
	if err != nil {
		return apiRegistration{}, err
	}
	var m struct {
		UserIDHex   string `json:"user_id_hex"`
		DeviceIDHex string `json:"device_id_hex"`
		DSKPubHex   string `json:"dsk_pub_hex"`
		TargetIDHex string `json:"target_id_hex"`
		NotAfter    uint64 `json:"not_after"`
		Groups      []struct {
			Name           string `json:"name"`
			GroupIDHex     string `json:"group_id_hex"`
			BindingHex     string `json:"binding_hex"`
			GroupInfoHex   string `json:"group_info_hex"`
			RatchetTreeHex string `json:"ratchet_tree_hex"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return apiRegistration{}, err
	}
	if uint64(time.Now().Unix()) >= m.NotAfter {
		return apiRegistration{}, errors.New("the fixture's leaf lifetimes have expired")
	}
	var out apiRegistration
	for _, f := range []struct {
		dst *id.ID
		src string
	}{{&out.userID, m.UserIDHex}, {&out.deviceID, m.DeviceIDHex}, {&out.targetID, m.TargetIDHex}} {
		if *f.dst, err = id.Parse(f.src); err != nil {
			return apiRegistration{}, err
		}
	}
	if out.dsk, err = hex.DecodeString(m.DSKPubHex); err != nil {
		return apiRegistration{}, err
	}
	out.groups = map[string]apiRegistrationGroup{}
	for _, g := range m.Groups {
		var rg apiRegistrationGroup
		if rg.groupID, err = id.Parse(g.GroupIDHex); err != nil {
			return apiRegistration{}, err
		}
		for _, f := range []struct {
			dst *[]byte
			src string
		}{{&rg.binding, g.BindingHex}, {&rg.groupInfo, g.GroupInfoHex}, {&rg.ratchetTree, g.RatchetTreeHex}} {
			if *f.dst, err = hex.DecodeString(f.src); err != nil {
				return apiRegistration{}, err
			}
		}
		out.groups[g.Name] = rg
	}
	return out, nil
}

// apiListEntry and apiListUnsigned are protocol/03-identity.md's device list, as
// internal/ds/devicelist_test.go encodes it.
type apiListEntry struct {
	_         struct{} `cbor:",toarray"`
	DeviceID  []byte
	DSKPub    []byte
	Tier      uint64
	AddedAt   uint64
	RevokedAt *uint64
}

type apiListUnsigned struct {
	_        struct{} `cbor:",toarray"`
	V        uint64
	UserID   []byte
	Version  uint64
	PrevHash []byte
	Entries  []apiListEntry
}

type apiListSigned struct {
	_        struct{} `cbor:",toarray"`
	V        uint64
	UserID   []byte
	Version  uint64
	PrevHash []byte
	Entries  []apiListEntry
	SigSSK   []byte
}

// registrantToken makes the registration fixture's creator a device of this instance - its user
// with an SSK, its device under the key its leaf carries, a signed device list that lists it - and
// answers an enrolled session for it. The fixture publishes no private key, so the session is
// minted as POST /v1/accounts mints a new device's first one (NewDeviceSession), as
// keyPackageOwnerToken does.
func (h *groupsAPI) registrantToken(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	f := apiRegistrationData(t)
	ssk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x74}, 32))
	entries := []apiListEntry{{DeviceID: f.deviceID[:], DSKPub: f.dsk, AddedAt: 1}}
	unsigned, err := cborx.Marshal(apiListUnsigned{
		V: 1, UserID: f.userID[:], Version: 1, PrevHash: make([]byte, 32), Entries: entries,
	})
	if err != nil {
		t.Fatalf("encode the unsigned list: %v", err)
	}
	sig := ed25519.Sign(ssk, append([]byte("dilla devices v1"), unsigned...))
	signed, err := cborx.Marshal(apiListSigned{
		V: 1, UserID: f.userID[:], Version: 1, PrevHash: make([]byte, 32), Entries: entries, SigSSK: sig,
	})
	if err != nil {
		t.Fatalf("encode the signed list: %v", err)
	}
	now := h.deps.Clock.Now().Unix()
	var token string
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateUser(ctx, store.UserRow{
			ID: f.userID, Username: "creator" + f.userID.String()[:8], Display: "Creator",
			UMKPub: make([]byte, 32), SSKPub: ssk.Public().(ed25519.PublicKey), SigUMKSSK: make([]byte, 64),
			Created: now,
		}); err != nil {
			return err
		}
		if err := tx.CreateDevice(ctx, store.DeviceRow{
			ID: f.deviceID, UserID: f.userID, DSKPub: f.dsk, CredentialBlob: []byte{1},
			LastSeen: now, Created: now,
		}); err != nil {
			return err
		}
		if err := tx.PutDeviceList(ctx, store.DeviceListRow{
			UserID: f.userID, Version: 1, Blob: signed, SSKSignature: sig,
			PrevHash: make([]byte, 32), Created: now,
		}); err != nil {
			return err
		}
		tok, err := h.deps.Sessions.NewDeviceSession(ctx, tx, f.userID, f.deviceID, 0)
		token = tok.Token
		return err
	}); err != nil {
		t.Fatalf("make the registration fixture's creator a device: %v", err)
	}
	return token
}

// oneLeafBody is the POST /v1/groups body for the registration fixture's group `name`, and
// that group's id.
func oneLeafBody(t *testing.T, name string) ([]byte, id.ID) {
	t.Helper()
	g, ok := apiRegistrationData(t).groups[name]
	if !ok {
		t.Fatalf("the registration fixture has no group %q", name)
	}
	return mustCBOR(t, []any{g.groupID, g.binding, g.groupInfo, g.ratchetTree}), g.groupID
}

// mustRegisterOneLeaf registers the fixture's one-leaf group over POST /v1/groups, as its creator,
// and answers the group id and the creator's token.
func (h *groupsAPI) mustRegisterOneLeaf(t *testing.T) (id.ID, string) {
	t.Helper()
	token := h.registrantToken(t)
	body, groupID := oneLeafBody(t, "one-leaf-0")
	if res := h.do(t, http.MethodPost, "/v1/groups", token, body); res.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	return groupID, token
}

// Hardening G over the wire: the 1,500-leaf fixture uploaded by the device of its own leaf 0 -
// registered under that leaf's key, listed - is refused with 400 E_INVALID_REQUEST and nothing is
// stored, while the same device's one-leaf group is accepted. (The DS-level cases are
// internal/ds/register_leaf_test.go.)
func TestTheRegistrationRouteAdoptsOnlyTheRegisteringDevicesOneLeaf(t *testing.T) {
	h := newGroupsAPI(t)
	token := h.registrantToken(t)
	res := h.do(t, http.MethodPost, "/v1/groups", token, h.createBody(t))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("a 1,500-leaf registration: status = %d, want 400: %s", res.Code, res.Body.String())
	}
	if got := errorCode(t, res); got != "E_INVALID_REQUEST" {
		t.Fatalf("code = %s, want E_INVALID_REQUEST", got)
	}
	if _, err := h.deps.Repo.GetGroup(context.Background(), h.groupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused registration left a group row: %v", err)
	}
	body, groupID := oneLeafBody(t, "one-leaf-0")
	if res := h.do(t, http.MethodPost, "/v1/groups", token, body); res.Code != http.StatusCreated {
		t.Fatalf("the honest registration: %d %s", res.Code, res.Body.String())
	}
	members, err := h.deps.Repo.ListMembers(context.Background(), groupID)
	if err != nil || len(members) != 1 || members[0].DeviceID != apiRegistrationData(t).deviceID {
		t.Fatalf("members = %+v, %v; want the registering device alone", members, err)
	}
}
