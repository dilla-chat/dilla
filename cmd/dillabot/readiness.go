//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/id"
)

type readinessSession interface {
	MarkReady(context.Context, id.ID, id.ID, uint64) (client.Installation, error)
}

// The server is authoritative for all required channel joins. Its ordinary
// conflict means prerequisites are still pending; a retry keeps the same ID.
func markReadyAfterMembership(ctx context.Context, session readinessSession, installation client.Installation) (bool, error) {
	if session == nil || installation.ID.IsZero() || installation.CommunityID.IsZero() || installation.Revision == 0 {
		return false, errors.New("invalid readiness binding")
	}
	if installation.State == 3 {
		return true, nil
	}
	if installation.State != 2 {
		return false, nil
	}
	seed := make([]byte, 0, 40)
	seed = append(seed, []byte("dilla.bot.ready.v1\x00")...)
	seed = append(seed, installation.ID[:]...)
	seed = append(seed, installation.CommunityID[:]...)
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], installation.Revision)
	seed = append(seed, revision[:]...)
	hash := sha256.Sum256(seed)
	var request id.ID
	copy(request[:], hash[:id.Size])
	row, err := session.MarkReady(ctx, installation.ID, request, installation.Revision)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && apiErr.Code == "E_INVALID_REQUEST" && apiErr.ReadinessConflict == "installation prerequisites are still pending" {
			return false, nil
		}
		return false, err
	}
	if row.ID != installation.ID || row.CommunityID != installation.CommunityID || row.State != 3 || row.Revision <= installation.Revision {
		return false, fmt.Errorf("invalid readiness response")
	}
	return true, nil
}
