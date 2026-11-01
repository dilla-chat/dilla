package sfu

import (
	"context"

	"github.com/livekit/protocol/livekit"

	"github.com/jonasthim/dilla/internal/id"
)

// Remote controls a LiveKit SFU in another process on this host. The same API key signs both
// room tokens and RoomService requests. Its HTTP listener must remain loopback-only: dillad's
// /rtc proxy is the admission gate for every browser join.
type Remote struct{ control *Server }

func NewRemote(cfg Config) *Remote { return &Remote{control: &Server{cfg: cfg}} }

func (r *Remote) HTTPURL() string                             { return r.control.HTTPURL() }
func (r *Remote) VerifyToken(token string) (RoomToken, error) { return r.control.VerifyToken(token) }
func (r *Remote) Token(room, identity string, perm *livekit.ParticipantPermission, attrs map[string]string) (string, error) {
	return r.control.Token(room, identity, perm, attrs)
}
func (r *Remote) Rooms(ctx context.Context) ([]string, error) { return r.control.Rooms(ctx) }
func (r *Remote) RoomSID(ctx context.Context, room string) (string, bool, error) {
	return r.control.RoomSID(ctx, room)
}
func (r *Remote) DeleteRoom(ctx context.Context, room string) error {
	return r.control.DeleteRoom(ctx, room)
}
func (r *Remote) CreateRoom(ctx context.Context, room string) error {
	return r.control.CreateRoom(ctx, room)
}
func (r *Remote) UpdatePermission(ctx context.Context, room, identity string, perm *livekit.ParticipantPermission) error {
	return r.control.UpdatePermission(ctx, room, identity, perm)
}
func (r *Remote) UpdateAttributes(ctx context.Context, room, identity string, attrs map[string]string) error {
	return r.control.UpdateAttributes(ctx, room, identity, attrs)
}
func (r *Remote) RemoveParticipants(ctx context.Context, room string, device id.ID) error {
	return r.control.RemoveParticipants(ctx, room, device)
}
func (r *Remote) RemoveParticipant(ctx context.Context, room, identity string) error {
	return r.control.RemoveParticipant(ctx, room, identity)
}
func (r *Remote) Participants(ctx context.Context, room string) ([]*livekit.ParticipantInfo, error) {
	return r.control.Participants(ctx, room)
}
