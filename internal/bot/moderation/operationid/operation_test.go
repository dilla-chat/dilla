package operationid

import (
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestAutomaticOperationIDIsStableAndSourceBound(t *testing.T) {
	group := id.New()
	first := Automatic(group, 7)
	if first.IsZero() || first != Automatic(group, 7) {
		t.Fatal("operation id is not stable for the same source")
	}
	if first == Automatic(group, 8) || first == Automatic(id.New(), 7) || !Automatic(group, 0).IsZero() {
		t.Fatal("different or invalid source reused an operation id")
	}
}
