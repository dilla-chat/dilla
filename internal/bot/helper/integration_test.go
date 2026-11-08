//go:build linux

package helper

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestCompiledHelperIntegration(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		if os.Getenv("DILLA_BOT_HELPER_INTEGRATION") == "1" {
			t.Fatal("DILLA_BOT_HELPER_INTEGRATION=1 requires DILLA_BOT_HELPER_BIN")
		}
		t.Skip("set DILLA_BOT_HELPER_BIN to run the compiled dilla-bot-core integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "private-state")
	instance, community, installation := realID(1), realID(2), realID(3)
	key := make([]byte, 32)
	copy(key, []byte("database-key-not-in-args-or-environment"))
	keySentinel := string(key)

	first := startReal(t, binary, state)
	if _, err := first.Open(context.Background(), instance, community, installation, append([]byte(nil), key...)); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if info, err := os.Stat(state); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("helper state directory mode = %v, %v; want 0700", info, err)
	}
	for _, name := range []string{"state.db", "state.lock"} {
		info, err := os.Stat(filepath.Join(state, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode = %v, %v; want 0600", name, info, err)
		}
	}

	wrongKey := append([]byte(nil), key...)
	wrongKey[0] ^= 0xff
	wrong := startReal(t, binary, state)
	_, err = wrong.Open(context.Background(), instance, community, installation, wrongKey)
	var status *StatusError
	if !errors.As(err, &status) || status.Status != 1 {
		t.Fatalf("wrong database key result = %v, want sanitized invalid status", err)
	}
	if wrongKey[0] != 0 || wrongKey[31] != 0 {
		t.Fatal("Open did not best-effort clear its key buffer")
	}
	if err := wrong.Close(); err != nil {
		t.Fatalf("wrong-key helper shutdown: %v", err)
	}

	reopened := startReal(t, binary, state)
	openKey := append([]byte(nil), key...)
	if _, err := reopened.Open(context.Background(), instance, community, installation, openKey); err != nil {
		t.Fatalf("reopen after wrong key: %v", err)
	}
	if openKey[0] != 0 || openKey[31] != 0 {
		t.Fatal("Open did not clear its key buffer")
	}
	instanceKey := [32]byte{0x77}
	args := []any{instance, community, installation, instanceKey[:]}
	one, err := reopened.Call(context.Background(), protocol.HelperBootstrap, args)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	two, err := reopened.Call(context.Background(), protocol.HelperBootstrap, args)
	if err != nil {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	if !reflect.DeepEqual(one, two) {
		t.Fatal("bootstrap result changed across repeated request")
	}
	account := realID(4)
	now := uint64(time.Now().Unix())
	completed, err := reopened.Call(context.Background(), protocol.HelperEnrollComplete, []any{account, "local integration bot", now})
	if err != nil {
		t.Fatalf("complete local enrollment: %v", err)
	}
	if len(completed) != 2 {
		t.Fatalf("enrollment result has %d values, want credential and signed device list", len(completed))
	}
	nonce := make([]byte, 32)
	nonce[0], nonce[31] = 0xa5, 0x5a
	signArgs := []any{uint64(1), nonce, uint64(0)}
	signed, err := reopened.Call(context.Background(), protocol.HelperSession, signArgs)
	if err != nil || len(signed) != 1 {
		t.Fatalf("session signing result = %#v, %v", signed, err)
	}
	expires, idle := now+3600, now+1800
	if _, err := reopened.Call(context.Background(), protocol.HelperSession, []any{uint64(2), "integration-session-sentinel", expires, idle}); err != nil {
		t.Fatalf("store session: %v", err)
	}
	stored, err := reopened.Call(context.Background(), protocol.HelperSession, []any{uint64(3)})
	if err != nil {
		t.Fatalf("read stored session: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	last := startReal(t, binary, state)
	if _, err := last.Open(context.Background(), instance, community, installation, append([]byte(nil), key...)); err != nil {
		t.Fatalf("reopen persisted state: %v", err)
	}
	process := last.transport.(*linuxExchange).cmd
	if strings.Contains(strings.Join(process.Args, "\x00"), keySentinel) || strings.Contains(strings.Join(process.Env, "\x00"), keySentinel) {
		t.Fatal("database key appeared in helper arguments or environment")
	}
	loaded, err := last.Call(context.Background(), protocol.HelperSession, []any{uint64(3)})
	if err != nil || !reflect.DeepEqual(stored, loaded) {
		t.Fatalf("persisted session = %#v (%v), before restart %#v", loaded, err, stored)
	}
	resigned, err := last.Call(context.Background(), protocol.HelperSession, signArgs)
	if err != nil || !reflect.DeepEqual(signed, resigned) {
		t.Fatalf("persisted identity signing changed: %#v (%v)", resigned, err)
	}
	groups, err := last.GroupApply(context.Background(), []any{uint64(0)})
	if err != nil || len(groups) != 1 || !reflect.DeepEqual(groups[0], []byte{0x80}) {
		t.Fatalf("empty enrolled Bot group list = %#v (%v), want an encoded empty CBOR array", groups, err)
	}
	if err := last.Close(); err != nil {
		t.Fatalf("final shutdown: %v", err)
	}
}

// TestOpenCommandRoundTripThroughHelper drives ops 21-24 on the compiled helper. A Go client
// verifies the command-key publication against the Bot's own signed device list, seals a
// command to it with RFC 9180 HPKE and signs it with its own device key, and opens the helper's
// sealed reply. Before the user's device list is pushed the command is refused as unlisted;
// after a first push (trust on first use of the user's UMK) the same command opens, once.
func TestOpenCommandRoundTripThroughHelper(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		if os.Getenv("DILLA_BOT_HELPER_INTEGRATION") == "1" {
			t.Fatal("DILLA_BOT_HELPER_INTEGRATION=1 requires DILLA_BOT_HELPER_BIN")
		}
		t.Skip("set DILLA_BOT_HELPER_BIN to run the compiled dilla-bot-core integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	instance, community, installation := realID(1), realID(2), realID(3)
	key := make([]byte, 32)
	copy(key, []byte("command-round-trip-database-key"))
	helper := startReal(t, binary, filepath.Join(t.TempDir(), "private-state"))
	if _, err := helper.Open(ctx, instance, community, installation, key); err != nil {
		t.Fatalf("open: %v", err)
	}
	instanceKey := [32]byte{0x77}
	if _, err := helper.Call(ctx, protocol.HelperBootstrap, []any{instance, community, installation, instanceKey[:]}); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	now := time.Now()
	enrolled, err := helper.Call(ctx, protocol.HelperEnrollComplete, []any{realID(4), "command bot", uint64(now.Unix())})
	if err != nil || len(enrolled) != 2 {
		t.Fatalf("enroll: %#v %v", enrolled, err)
	}
	botDevice, botDSK := ownDeviceKey(t, enrolled[1])

	publication, err := helper.CommandKey(ctx, community, now)
	if err != nil {
		t.Fatalf("CommandKey: %v", err)
	}
	again, err := helper.CommandKey(ctx, community, now.Add(time.Minute))
	if err != nil || !bytes.Equal(again, publication) {
		t.Fatalf("CommandKey is not stable within the week: %v", err)
	}
	if _, err := helper.CommandKey(ctx, realID(9), now); err == nil {
		t.Fatal("CommandKey minted a key for a community the helper is not bound to")
	}
	keyID, commandKey := verifyPublication(t, publication, community, botDevice, botDSK, now)

	user, device := realID(5), realID(6)
	_, userKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	requestID := realID(7)
	sealed := sealTestCommand(t, commandKey, keyID, requestID, community, user, device, userKey, reply.PublicKey().Bytes(), uint64(now.Unix()))

	opened, err := helper.OpenCommand(ctx, sealed, user, device, now)
	if err != nil {
		t.Fatalf("OpenCommand: %v", err)
	}
	if opened.Refusal != RefusalDeviceUnlisted {
		t.Fatalf("command from a user with no cached list = %d, want RefusalDeviceUnlisted", opened.Refusal)
	}
	push := testDeviceListPush(t, user, device, userKey.Public().(ed25519.PublicKey))
	if changed, version, err := helper.IngestDeviceList(ctx, user, push); err != nil || !changed || version != 1 {
		t.Fatalf("first push = %v %d %v, want a pinned version 1", changed, version, err)
	}
	if changed, version, err := helper.IngestDeviceList(ctx, user, push); err != nil || changed || version != 1 {
		t.Fatalf("repeated push = %v %d %v, want unchanged", changed, version, err)
	}
	opened, err = helper.OpenCommand(ctx, sealed, user, device, now)
	if err != nil || opened.Refusal != RefusalNone {
		t.Fatalf("command after the push = %#v, %v", opened, err)
	}
	r := opened.Request
	if r.RequestID != requestID || r.CommunityID != community || r.ChannelID != (id.ID{}) || r.Operation != 7 ||
		r.SentAt != uint64(now.Unix()) || !bytes.Equal(r.ReplyKey[:], reply.PublicKey().Bytes()) ||
		r.UserID != user || r.DeviceID != device || len(r.Args) != 2 || r.Args[0] != "hello" || r.Args[1] != uint64(3) {
		t.Fatalf("opened request = %#v", r)
	}
	if again, err := helper.OpenCommand(ctx, sealed, user, device, now); err != nil || again.Refusal != RefusalReplay {
		t.Fatalf("replayed command = %#v, %v; want RefusalReplay", again, err)
	}
	if other, err := helper.OpenCommand(ctx, sealed, user, realID(10), now); err != nil || other.Refusal != RefusalWrongDevice {
		t.Fatalf("command attached to another device = %#v, %v; want RefusalWrongDevice", other, err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	if opened, err := helper.OpenCommand(ctx, tampered, user, device, now); err != nil || opened.Refusal != RefusalMalformed {
		t.Fatalf("tampered seal = %#v, %v; want RefusalMalformed", opened, err)
	}
	var otherKey [16]byte
	otherKey[0] = 0xee
	unknown := sealTestCommand(t, commandKey, otherKey[:], realID(8), community, user, device, userKey, reply.PublicKey().Bytes(), uint64(now.Unix()))
	if opened, err := helper.OpenCommand(ctx, unknown, user, device, now); err != nil || opened.Refusal != RefusalUnknownKey {
		t.Fatalf("unknown key id = %#v, %v; want RefusalUnknownKey", opened, err)
	}

	var replyKey [32]byte
	copy(replyKey[:], reply.PublicKey().Bytes())
	sealedReply, err := helper.SealReply(ctx, replyKey, requestID, 3, []byte("Now playing"))
	if err != nil {
		t.Fatalf("SealReply: %v", err)
	}
	if got := openTestReply(t, reply, requestID, 3, sealedReply); string(got) != "Now playing" {
		t.Fatalf("opened reply = %q", got)
	}

	changed, version, err := helper.IngestDeviceList(ctx, realID(11), []byte{0x80})
	var status *StatusError
	if !errors.As(err, &status) || changed || version != 0 {
		t.Fatalf("IngestDeviceList of a malformed push = %v %d %v, want a definitive refusal", changed, version, err)
	}
	if err := helper.Close(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// ownDeviceKey reads the Bot's device id and DSK from its signed device-list PUT body
// `[version, blob, ssk_signature, prev_hash]`, blob `[v, user, version, prev_hash, entries, sig]`.
// TestProbeRoundTripThroughHelper (Bot v2 task 17, fix round 2) drives ops 25 and 26 on the compiled helper with ops
// 21-24: the probe is sealed to the Bot's own current key as its own device, opened by op 22 against the Bot's own list
// as dillad's feed pushes it, answered through op 23, and the answer opens with the handle once.
func TestProbeRoundTripThroughHelper(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		if os.Getenv("DILLA_BOT_HELPER_INTEGRATION") == "1" {
			t.Fatal("DILLA_BOT_HELPER_INTEGRATION=1 requires DILLA_BOT_HELPER_BIN")
		}
		t.Skip("set DILLA_BOT_HELPER_BIN to run the compiled dilla-bot-core integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	instance, community, installation, botUser := realID(1), realID(2), realID(3), realID(4)
	key := make([]byte, 32)
	copy(key, []byte("probe-round-trip-database-key"))
	helper := startReal(t, binary, filepath.Join(t.TempDir(), "private-state"))
	if _, err := helper.Open(ctx, instance, community, installation, key); err != nil {
		t.Fatalf("open: %v", err)
	}
	instanceKey := [32]byte{0x77}
	registration, err := helper.Call(ctx, protocol.HelperBootstrap, []any{instance, community, installation, instanceKey[:]})
	if err != nil || len(registration) != 4 {
		t.Fatalf("bootstrap: %#v %v", registration, err)
	}
	now := time.Now()
	enrolled, err := helper.Call(ctx, protocol.HelperEnrollComplete, []any{botUser, "probe bot", uint64(now.Unix())})
	if err != nil || len(enrolled) != 2 {
		t.Fatalf("enroll: %#v %v", enrolled, err)
	}
	botDevice, _ := ownDeviceKey(t, enrolled[1])
	var device id.ID
	copy(device[:], botDevice)

	// No key yet: the probe does not mint one.
	if _, _, err := helper.SealProbe(ctx, community, realID(20), now); err == nil {
		t.Fatal("a probe was sealed before any command key existed")
	}
	if _, err := helper.CommandKey(ctx, community, now); err != nil {
		t.Fatalf("CommandKey: %v", err)
	}
	request := realID(21)
	sealed, _, err := helper.SealProbe(ctx, community, request, now)
	if err != nil {
		t.Fatalf("SealProbe: %v", err)
	}
	// Before the Bot's own list is in the cache the probe is refused like any unlisted sender's command.
	if opened, err := helper.OpenCommand(ctx, sealed, botUser, device, now); err != nil || opened.Refusal != RefusalDeviceUnlisted {
		t.Fatalf("probe before the Bot's own list = %#v, %v", opened, err)
	}
	var row []any
	if raw, _ := enrolled[1].([]byte); cborx.Unmarshal(raw, &row) != nil || len(row) != 4 {
		t.Fatalf("device list body = %#v", enrolled[1])
	}
	push := protocol.DeviceListPush{History: []protocol.DeviceListRow{{Version: row[0].(uint64), Blob: row[1].([]byte)}}}
	copy(push.History[0].SSKSignature[:], row[2].([]byte))
	copy(push.History[0].PrevHash[:], row[3].([]byte))
	copy(push.UMK[:], registration[0].([]byte))
	copy(push.SSK[:], registration[1].([]byte))
	copy(push.SigUMKSSK[:], registration[2].([]byte))
	body, err := protocol.EncodeDeviceListPush(push)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := helper.IngestDeviceList(ctx, botUser, body); err != nil {
		t.Fatalf("the Bot's own list: %v", err)
	}
	request = realID(22)
	sealed, handle, err := helper.SealProbe(ctx, community, request, now)
	if err != nil {
		t.Fatalf("SealProbe: %v", err)
	}
	opened, err := helper.OpenCommand(ctx, sealed, botUser, device, now)
	if err != nil || opened.Refusal != RefusalNone {
		t.Fatalf("probe = %#v, %v", opened, err)
	}
	if r := opened.Request; r.RequestID != request || r.CommunityID != community || r.Operation != uint64(protocol.OpProbe) || len(r.Args) != 0 {
		t.Fatalf("opened probe = %#v", r)
	}
	answer, err := helper.SealReply(ctx, opened.Request.ReplyKey, request, 0, []byte("ok"))
	if err != nil {
		t.Fatalf("SealReply: %v", err)
	}
	if got, err := helper.OpenProbeReply(ctx, handle, 0, answer, true); err != nil || string(got) != "ok" {
		t.Fatalf("OpenProbeReply = %q, %v", got, err)
	}
	if _, err := helper.OpenProbeReply(ctx, handle, 0, answer, true); err == nil {
		t.Fatal("a forgotten probe secret opened again")
	}
}

func ownDeviceKey(t *testing.T, body any) ([]byte, ed25519.PublicKey) {
	t.Helper()
	raw, ok := body.([]byte)
	var row, list []any
	if !ok || cborx.Unmarshal(raw, &row) != nil || len(row) != 4 {
		t.Fatalf("device list body = %#v", body)
	}
	blob, _ := row[1].([]byte)
	if cborx.Unmarshal(blob, &list) != nil || len(list) != 6 {
		t.Fatalf("device list blob = %x", blob)
	}
	entries, _ := list[4].([]any)
	if len(entries) != 1 {
		t.Fatalf("device list entries = %#v", list[4])
	}
	entry, _ := entries[0].([]any)
	device, _ := entry[0].([]byte)
	dsk, _ := entry[1].([]byte)
	if len(device) != 16 || len(dsk) != ed25519.PublicKeySize {
		t.Fatalf("device entry = %#v", entry)
	}
	return device, ed25519.PublicKey(dsk)
}

// verifyPublication checks `[1, community_id, key_id, public_key, not_after, bot_device_id, sig]`
// the way a client does, and that a client 5 minutes behind still accepts it.
func verifyPublication(t *testing.T, publication []byte, community id.ID, botDevice []byte, botDSK ed25519.PublicKey, now time.Time) ([]byte, []byte) {
	t.Helper()
	var p []any
	if err := cborx.Unmarshal(publication, &p); err != nil || len(p) != 7 || p[0] != uint64(1) {
		t.Fatalf("publication = %#v, %v", p, err)
	}
	keyID, _ := p[2].([]byte)
	commandKey, _ := p[3].([]byte)
	notAfter, _ := p[4].(uint64)
	sig, _ := p[6].([]byte)
	if !bytes.Equal(p[1].([]byte), community[:]) || !bytes.Equal(p[5].([]byte), botDevice) || len(keyID) != 16 || len(commandKey) != 32 {
		t.Fatalf("publication fields = %#v", p)
	}
	unsigned, err := cborx.Marshal(p[:6])
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(botDSK, append([]byte("dilla bot command key v1"), unsigned...), sig) {
		t.Fatal("publication signature does not verify under the Bot's listed device key")
	}
	const maxLifetime = 8 * 24 * 60 * 60
	slow := uint64(now.Add(-5 * time.Minute).Unix())
	if notAfter <= uint64(now.Unix()) || notAfter > slow+maxLifetime {
		t.Fatalf("not_after %d is outside a 5-minute-slow client's 8-day cap", notAfter)
	}
	return keyID, commandKey
}

// The sealed-command suite: DHKEM(X25519, HKDF-SHA256) through the X25519 keys, HKDF-SHA256,
// AES-128-GCM.
var commandSuite = struct {
	kdf  hpke.KDF
	aead hpke.AEAD
}{hpke.HKDFSHA256(), hpke.AES128GCM()}

func sealTestCommand(t *testing.T, commandKey, keyID []byte, requestID, community, user, device id.ID, userKey ed25519.PrivateKey, replyKey []byte, sentAt uint64) []byte {
	t.Helper()
	fields := []any{uint64(1), requestID[:], community[:], nil, uint64(7), []any{"hello", uint64(3)}, sentAt, replyKey, keyID, user[:], device[:]}
	unsigned, err := cborx.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := cborx.Marshal(append(fields, ed25519.Sign(userKey, append([]byte("dilla bot command v1"), unsigned...))))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ecdh.X25519().NewPublicKey(commandKey)
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := hpke.NewDHKEMPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	enc, sender, err := hpke.NewSender(recipient, commandSuite.kdf, commandSuite.aead, []byte("dilla bot command v1"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sender.Seal(requestID[:], signed)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cborx.Marshal([]any{uint64(1), keyID, requestID[:], enc, ciphertext})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// testDeviceListPush builds a first push for user: a fresh UMK certifying a fresh SSK, and a
// version-1 list naming device with dsk, as protocol.DeviceListPush.
func testDeviceListPush(t *testing.T, user, device id.ID, dsk ed25519.PublicKey) []byte {
	t.Helper()
	umkPub, umk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sskPub, ssk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	prev := make([]byte, 32)
	unsigned := []any{uint64(1), user[:], uint64(1), prev, []any{[]any{device[:], []byte(dsk), uint64(0), uint64(100), nil}}}
	message, err := cborx.Marshal(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(ssk, append([]byte("dilla devices v1"), message...))
	blob, err := cborx.Marshal(append(unsigned, sig))
	if err != nil {
		t.Fatal(err)
	}
	p := protocol.DeviceListPush{History: []protocol.DeviceListRow{{Version: 1, Blob: blob}}}
	copy(p.UMK[:], umkPub)
	copy(p.SSK[:], sskPub)
	copy(p.SigUMKSSK[:], ed25519.Sign(umk, append([]byte("dilla ssk v1"), sskPub...)))
	copy(p.History[0].SSKSignature[:], sig)
	body, err := protocol.EncodeDeviceListPush(p)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func openTestReply(t *testing.T, reply *ecdh.PrivateKey, requestID id.ID, seq uint32, sealed []byte) []byte {
	t.Helper()
	var r []any
	if err := cborx.Unmarshal(sealed, &r); err != nil || len(r) != 3 || r[0] != uint64(1) {
		t.Fatalf("sealed reply = %#v, %v", r, err)
	}
	enc, _ := r[1].([]byte)
	ciphertext, _ := r[2].([]byte)
	key, err := hpke.NewDHKEMPrivateKey(reply)
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := hpke.NewRecipient(enc, key, commandSuite.kdf, commandSuite.aead, []byte("dilla bot reply v1"))
	if err != nil {
		t.Fatal(err)
	}
	aad := binary.BigEndian.AppendUint32(append([]byte(nil), requestID[:]...), seq)
	body, err := recipient.Open(aad, ciphertext)
	if err != nil {
		t.Fatalf("open reply: %v", err)
	}
	return body
}

func startReal(t *testing.T, executable, state string) *Client {
	t.Helper()
	client, err := Start(Options{Executable: executable, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func realID(n byte) id.ID { var value id.ID; value[15] = n; return value }
