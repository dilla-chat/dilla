package service

import (
	"testing"
	"time"
)

// NextDue is the earliest backed-off device of one installation (Bot v2 task 14 fix round 1, M-3).
func TestRegistrationBackoffNextDueIsPerInstallation(t *testing.T) {
	b := NewInteractionRegistrationBackoff()
	now := time.Unix(1_800_000_000, 0)
	install, other := testID(1), testID(2)
	if _, ok := b.NextDue(install); ok {
		t.Fatal("an empty backoff named a due time")
	}
	b.fail(interactionRegistrationBackoffKey{installation: install, user: testID(3), episode: testID(4), device: testID(5)}, now)
	b.fail(interactionRegistrationBackoffKey{installation: other, user: testID(3), episode: testID(4), device: testID(6)}, now.Add(-time.Hour))
	got, ok := b.NextDue(install)
	if !ok || !got.Equal(now.Add(interactionRegistrationBackoffBase)) {
		t.Fatalf("NextDue = %v, %v; want %v", got, ok, now.Add(interactionRegistrationBackoffBase))
	}
}
