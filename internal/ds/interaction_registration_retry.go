package ds

import (
	"bytes"
	"errors"

	"github.com/dilla-chat/dilla/internal/store"
)

var errKind3RegistrationReplayConflict = errors.New("ds: kind-3 registration replay does not match its durable admission consumption")

// verifyKind3RegistrationReplay accepts a retry only when the operation receipt matches the
// request and the durable consumed-admission row points to that exact receipt. The consumed row
// remains available after the signed admission payload expires or is compacted.
func verifyKind3RegistrationReplay(operation, consumed, expected store.BotGroupRegistrationReceipt) error {
	if !sameKind3RegistrationIdentity(operation, expected) || !sameKind3RegistrationIdentity(consumed, expected) || !sameKind3RegistrationReceipt(operation, consumed) {
		return errKind3RegistrationReplayConflict
	}
	return nil
}

func sameKind3RegistrationIdentity(a, b store.BotGroupRegistrationReceipt) bool {
	return a.InstallationID == b.InstallationID && a.OperationID == b.OperationID && a.UserID == b.UserID && a.DeviceID == b.DeviceID && a.CommunityID == b.CommunityID && a.GroupID == b.GroupID && bytes.Equal(a.BodySHA256, b.BodySHA256) && a.Generation == b.Generation && a.Epoch == b.Epoch && a.NextSeq == b.NextSeq
}

func sameKind3RegistrationReceipt(a, b store.BotGroupRegistrationReceipt) bool {
	return sameKind3RegistrationIdentity(a, b) && a.AcceptedAt == b.AcceptedAt
}
