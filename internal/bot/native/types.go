package native

import (
	"encoding/json"
	"fmt"

	"github.com/dilla-chat/dilla/internal/id"
)

type DeviceRegistration struct {
	DeviceID   [16]byte
	DSKPublic  [32]byte
	Tier       uint64
	SignerTier uint64
	Credential []byte
}

type BootstrapResult struct {
	UMKPublic [32]byte
	SSKPublic [32]byte
	SigUMKSSK [64]byte
	Device    DeviceRegistration
}

type EnrollmentResult struct{ Credential, DeviceListBody []byte }

type Identity struct {
	Phase                  uint64
	Instance, User, Device *id.ID
	Username               string
	DeviceListPublished    bool
}

type SessionSignature struct {
	Nonce     [32]byte
	Purpose   uint64
	Signature [64]byte
}

type CallMediaMember struct {
	Leaf     uint64
	DeviceID id.ID
	UserID   id.ID
}

// CallMediaEpoch contains per-call key material. Standard formatting redacts it.
type CallMediaEpoch struct {
	Epoch   uint64
	BaseKey [16]byte
	OwnLeaf uint64
	Roster  []CallMediaMember
	Code    string
}

func (CallMediaEpoch) String() string   { return "CallMediaEpoch{base_key:[redacted]}" }
func (CallMediaEpoch) GoString() string { return "native.CallMediaEpoch{base_key:[redacted]}" }

func (e CallMediaEpoch) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Epoch   uint64            `json:"epoch"`
		BaseKey string            `json:"base_key"`
		OwnLeaf uint64            `json:"own_leaf"`
		Roster  []CallMediaMember `json:"roster"`
		Code    string            `json:"code"`
	}{e.Epoch, "[redacted]", e.OwnLeaf, e.Roster, e.Code})
}

var _ fmt.Stringer = CallMediaEpoch{}

// Session keeps the bearer private and explicitly redacts all standard fmt
// diagnostics. Token is the intentional access path for authenticated HTTP.
type Session struct {
	token                string
	Expires, IdleExpires uint64
}

func (s Session) Token() string  { return s.token }
func (Session) String() string   { return "Session{token:[redacted]}" }
func (Session) GoString() string { return "native.Session{token:[redacted]}" }

var _ fmt.Stringer = Session{}
