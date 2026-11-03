package ds

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

func TestAuthenticatedBotInteractionBindingUsesImportedGroupContext(t *testing.T) {
	ctx := context.Background()
	module, err := os.ReadFile(filepath.Join("..", "mlswasi", "testdata", "dilla_core_wasi.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	wasm, err := mlswasi.New(ctx, module, mlswasi.Options{PoolSize: 1, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer wasm.Close(ctx)
	tree, info, meta := readInteractionFixture(t, "")
	groupID := fixtureID(t, "group", meta.GroupIDHex)
	binding := fixtureBytes(t, "binding", meta.BindingHex)
	d := &DS{opts: Options{Wasm: wasm}}
	got, err := d.AuthenticatedBotInteractionBinding(ctx, RegisterRequest{GroupID: groupID, Binding: binding, GroupInfo: info, RatchetTree: tree})
	if err != nil {
		t.Fatalf("authenticated binding: %v", err)
	}
	if got.InstanceID != fixtureID(t, "instance", meta.InstanceIDHex) || got.BotUserID != fixtureID(t, "bot user", meta.BotUserIDHex) ||
		got.InstallationID != fixtureID(t, "installation", meta.InstallationIDHex) || got.CommunityID != fixtureID(t, "community", meta.CommunityIDHex) ||
		got.ParticipantUserID != fixtureID(t, "participant user", meta.ParticipantUserIDHex) || got.ParticipantDeviceID != fixtureID(t, "participant device", meta.ParticipantDeviceIDHex) {
		t.Fatalf("authenticated routing hints = %+v", got)
	}
	bad := append([]byte(nil), binding...)
	bad[len(bad)-1] ^= 1
	if _, err := d.AuthenticatedBotInteractionBinding(ctx, RegisterRequest{GroupID: groupID, Binding: bad, GroupInfo: info, RatchetTree: tree}); err == nil {
		t.Fatal("binding differing from authenticated GroupContext accepted")
	}
	if _, err := d.AuthenticatedBotInteractionBinding(ctx, RegisterRequest{GroupID: id.New(), Binding: binding, GroupInfo: info, RatchetTree: tree}); err == nil {
		t.Fatal("different group id accepted as routing context")
	}
}
