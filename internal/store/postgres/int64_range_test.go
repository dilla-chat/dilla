package postgres

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestUnsignedInputsRejectValuesAboveInt64(t *testing.T) {
	repo, _ := newPostgresRepo(t)
	groupID, channelID := id.New(), id.New()
	tooLarge := uint64(math.MaxInt64) + 1
	checks := []struct {
		name string
		call func() error
	}{
		{"SetGeneration", func() error { return repo.SetGeneration(t.Context(), tooLarge) }},
		{"ListHandshakes", func() error { _, err := repo.ListHandshakes(t.Context(), groupID, tooLarge, 1); return err }},
		{"ListAppMessages", func() error { _, err := repo.ListAppMessages(t.Context(), groupID, tooLarge, 1); return err }},
		{"GetAppMessage", func() error { _, err := repo.GetAppMessage(t.Context(), groupID, tooLarge); return err }},
		{"ListReadableMessages", func() error { _, err := repo.ListReadableMessages(t.Context(), channelID, tooLarge, 1); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			err := check.call()
			if err == nil || !strings.Contains(err.Error(), "exceeds int64 range") {
				t.Fatalf("error = %v, want int64 range error", err)
			}
		})
	}
	if err := repo.SetGeneration(t.Context(), math.MaxInt64); err != nil {
		t.Fatalf("SetGeneration(MaxInt64): %v", err)
	}
	if _, err := repo.ListHandshakes(t.Context(), groupID, math.MaxInt64, 1); err != nil {
		t.Fatalf("ListHandshakes(MaxInt64): %v", err)
	}
	if _, err := repo.ListAppMessages(t.Context(), groupID, math.MaxInt64, 1); err != nil {
		t.Fatalf("ListAppMessages(MaxInt64): %v", err)
	}
	if _, err := repo.GetAppMessage(t.Context(), groupID, math.MaxInt64); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAppMessage(MaxInt64) error = %v, want ErrNotFound", err)
	}
	if _, err := repo.ListReadableMessages(t.Context(), channelID, math.MaxInt64, 1); err != nil {
		t.Fatalf("ListReadableMessages(MaxInt64): %v", err)
	}
}
