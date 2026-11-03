package protocol

import (
	"crypto/sha256"
	"math"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const MaxGroupRegistrationBody = 65_505

var groupRegistrationDomain = []byte("dilla bot group registration v1\x00")

type GroupRegistrationRequest struct {
	OperationID        id.ID
	ExpectedGeneration uint64
	RegistrationBody   []byte
}

type GroupRegistrationReceipt struct {
	OperationID        id.ID
	GroupID            id.ID
	RegistrationSHA256 [32]byte
	AcceptedGeneration uint64
	InitialEpoch       uint64
	NextSequence       uint64
}

func GroupRegistrationGroupID(body []byte) (id.ID, error) {
	var zero id.ID
	if len(body) == 0 || len(body) > MaxGroupRegistrationBody {
		return zero, ErrLimit
	}
	n, err := parse(body)
	if err != nil || n.major != 4 || len(n.a) != 4 || n.a[0].major != 2 || len(n.a[0].b) != 16 || n.a[1].major != 2 || n.a[2].major != 2 || n.a[3].major != 2 {
		return zero, invalid()
	}
	group := nodeID(n.a[0])
	if group.IsZero() {
		return zero, invalid()
	}
	return group, nil
}

func GroupRegistrationHash(body []byte) ([32]byte, error) {
	if _, err := GroupRegistrationGroupID(body); err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	_, _ = h.Write(groupRegistrationDomain)
	_, _ = h.Write(body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func EncodeGroupRegistrationRequest(v GroupRegistrationRequest) ([]byte, error) {
	if v.OperationID.IsZero() || v.ExpectedGeneration > math.MaxInt64 {
		return nil, invalid()
	}
	if _, err := GroupRegistrationGroupID(v.RegistrationBody); err != nil {
		return nil, invalid()
	}
	b, err := cborx.Marshal([]any{v.OperationID, v.ExpectedGeneration, v.RegistrationBody})
	if err != nil {
		return nil, invalid()
	}
	if len(b) > MaxGroupRegistrationBody+64 {
		return nil, ErrLimit
	}
	return b, nil
}

func DecodeGroupRegistrationRequest(b []byte) (GroupRegistrationRequest, error) {
	var z GroupRegistrationRequest
	if len(b) > MaxGroupRegistrationBody+64 {
		return z, ErrLimit
	}
	n, err := parse(b)
	if err != nil || n.major != 4 || len(n.a) != 3 || n.a[0].major != 2 || len(n.a[0].b) != 16 || n.a[1].major != 0 || n.a[2].major != 2 {
		return z, invalid()
	}
	v := GroupRegistrationRequest{OperationID: nodeID(n.a[0]), ExpectedGeneration: n.a[1].u, RegistrationBody: append([]byte(nil), n.a[2].b...)}
	if v.OperationID.IsZero() || v.ExpectedGeneration > math.MaxInt64 {
		return z, invalid()
	}
	if _, err := GroupRegistrationGroupID(v.RegistrationBody); err != nil {
		return z, invalid()
	}
	return v, nil
}

func EncodeGroupRegistrationReceipt(v GroupRegistrationReceipt) ([]byte, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	return cborx.Marshal([]any{v.OperationID, v.GroupID, v.RegistrationSHA256[:], v.AcceptedGeneration, v.InitialEpoch, v.NextSequence})
}

func DecodeGroupRegistrationReceipt(b []byte) (GroupRegistrationReceipt, error) {
	var z GroupRegistrationReceipt
	n, err := parse(b)
	if err != nil || n.major != 4 || len(n.a) != 6 || n.a[0].major != 2 || len(n.a[0].b) != 16 || n.a[1].major != 2 || len(n.a[1].b) != 16 || n.a[2].major != 2 || len(n.a[2].b) != 32 {
		return z, invalid()
	}
	for _, i := range []int{3, 4, 5} {
		if n.a[i].major != 0 {
			return z, invalid()
		}
	}
	v := GroupRegistrationReceipt{OperationID: nodeID(n.a[0]), GroupID: nodeID(n.a[1]), AcceptedGeneration: n.a[3].u, InitialEpoch: n.a[4].u, NextSequence: n.a[5].u}
	copy(v.RegistrationSHA256[:], n.a[2].b)
	if err := v.validate(); err != nil {
		return z, err
	}
	return v, nil
}

func (v GroupRegistrationReceipt) validate() error {
	if v.OperationID.IsZero() || v.GroupID.IsZero() || v.AcceptedGeneration > math.MaxInt64 || v.InitialEpoch > math.MaxInt64 || v.NextSequence != 1 {
		return invalid()
	}
	return nil
}
