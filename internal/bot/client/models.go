package client

import "github.com/jonasthim/dilla/internal/id"

type Discovery struct {
	WireVersions, E2EEVersions, MediaVersions []uint64
	InstanceID                                id.ID
	Generation                                uint64
	Name                                      string
	RegistrationMode                          uint64
	AuthMethods                               []uint64
	PolicyVersion                             uint64
	ExternalSenderKeyID                       id.ID
	ExternalSenderPub                         []byte
}
type Limits struct{ Values [11]uint64 }

// LegacyServerCapabilities are the capability bits (8 Polls, 9 Reminders) a server that answers
// the eight-field installation cannot switch off; the runtime list treats them as on.
const LegacyServerCapabilities uint64 = 1<<8 | 1<<9

type Installation struct {
	ID, CommunityID         id.ID
	State, Revision         uint64
	BotUser, BotDevice      *id.ID
	Capabilities, UpdatedAt uint64
	// Task 10c, present only when the server answered the widened shape (Widened): the manager's
	// restart request counter and the last one this installation's runtime finished, the version
	// the runtime last reported, and the Music page's defaults.
	Widened                          bool
	RestartRequested, RestartHandled uint64
	RuntimeVersion                   *string
	MusicVolume, MusicIdleMinutes    uint64
}

// KeyPackagePublicationReceipt is the installation's historical acceptance result. It does not
// claim that any of its packages remain active in the directory.
type KeyPackagePublicationReceipt struct {
	Generation   uint64
	UploadSHA256 [32]byte
	Count        uint64
}
type Claim struct {
	InstallationID, ClaimID id.ID
	Expires                 uint64
}
type Enrollment struct {
	BotUser, BotDevice id.ID
	Username           string
}
type SessionChallenge struct {
	Nonce   []byte
	Expires uint64
}
type DeviceSession struct {
	Token                            string
	Scope                            uint64
	UserID, DeviceID                 id.ID
	Expires, IdleExpires, Generation uint64
}
type Negotiated struct{ Wire, E2EE, Media uint64 }
type GroupInfo struct {
	Epoch        uint64
	Info         []byte
	TreeHash     []byte
	NextSequence uint64
}
type GroupTree struct {
	Epoch    uint64
	Tree     []byte
	TreeHash []byte
}
type SequenceResult struct{ Sequence, Epoch uint64 }
type HealStatus struct{ Epoch, NextSequence, Generation, NeedFromSequence uint64 }
type InteractionHealStatus struct {
	HealStatus
	NeedsHeal bool
}
type HealResult struct{ Epoch, NextSequence uint64 }
type MessageResult struct {
	Sequence    uint64
	FrankingTag []byte
	ReceivedAt  uint64
}
type Page struct {
	Rows [][]any
	Next uint64
}
type Welcome struct {
	ID                    uint64
	GroupID               id.ID
	Epoch, CommitSequence uint64
	Blob, Tree, TreeHash  []byte
}
type Community struct {
	ID            id.ID
	Name          string
	OwnerID       id.ID
	PolicyVersion uint64
}
type Channel struct {
	ID, CommunityID              id.ID
	Kind, Mode, Visibility       uint64
	ParentID                     *id.ID
	Name, Topic                  string
	Position, Slowmode, Sequence uint64
	TextGroupID                  *id.ID
}
type DeviceRegistration struct {
	DeviceID         id.ID
	DSKPub           []byte
	Tier, SignerTier uint64
	Credential       []byte
}
