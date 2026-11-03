//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestFormatInteractionRecoveryDiagnostics(t *testing.T) {
	var out bytes.Buffer
	row := service.InteractionRegistrationRecoveryDiagnostic{Installation: id.New(), Operation: id.New(), Group: id.New(), Generation: 17, Phase: 0, ReceiptStatus: "present", GroupStatus: "absent"}
	row.RegistrationSHA256[0] = 0xab
	if err := formatInteractionRecoveryDiagnostics(&out, []service.InteractionRegistrationRecoveryDiagnostic{row}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"installation=" + row.Installation.String(), "operation=" + row.Operation.String(), "group=" + row.Group.String(), "generation=17", "registration_sha256=ab", "receipt=present", "group_status=absent"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("diagnostic output lacks %q: %s", want, out.String())
		}
	}
}

func TestOpenExistingInteractionStateKeyDoesNotCreateMissingState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created")
	key, exists, err := openExistingInteractionStateKey(root, id.New().String())
	if err != nil || exists || key != nil {
		t.Fatalf("open missing state = key:%v exists:%v err:%v", key, exists, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("diagnostic created missing state directory: stat error=%v", err)
	}
}

func TestFormatInteractionRecoveryDiagnosticsEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := formatInteractionRecoveryDiagnostics(&out, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "No manual-recovery interaction registrations." {
		t.Fatalf("empty diagnostic output = %q", got)
	}
}
