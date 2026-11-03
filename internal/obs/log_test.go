package obs_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/obs"
)

func TestRedactorReplacesSecretKeysAtEveryDepth(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger(config.Log{Level: "info", Format: "json"}, &buf)
	log.Info("login",
		slog.String("token", "supersecret"),
		slog.String("username", "jonas"),
		slog.Group("inner",
			slog.String("password", "hunter2"),
			slog.String("route", "/v1/auth/password/login"),
			slog.Group("deeper", slog.String("phc", "$argon2id$v=19$m=19456,t=2,p=1$abc$def"))),
	)
	out := buf.String()
	for _, secret := range []string{"supersecret", "hunter2", "argon2id"} {
		if strings.Contains(out, secret) {
			t.Fatalf("redactor let %q through: %s", secret, out)
		}
	}
	for _, kept := range []string{"jonas", "/v1/auth/password/login"} {
		if !strings.Contains(out, kept) {
			t.Fatalf("redactor dropped a non-secret attribute %q: %s", kept, out)
		}
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &line); err != nil {
		t.Fatalf("output is not one JSON object: %v", err)
	}
}

func TestAConfigNeverReachesTheLogWithItsSecrets(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger(config.Log{Level: "info", Format: "json"}, &buf)
	c := config.Default()
	c.DB.DSN = "postgres://dilla:hunter2@localhost/dilla"
	log.Info("starting", slog.Any("config", c.Redacted()))
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("a config secret reached the log: %s", buf.String())
	}
}

func TestIdentifiersAreLoggedAsAnEightHexPrefix(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger(config.Log{Level: "info", Format: "json"}, &buf)
	full := "0123456789abcdef0123456789abcdef"
	log.Info("group", slog.String("group_id", full))
	if strings.Contains(buf.String(), full) {
		t.Fatalf("a full identifier reached the log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "01234567") {
		t.Fatalf("the 8-hex prefix is missing: %s", buf.String())
	}
}

// idValue is an identifier type that logs through fmt.Stringer, as id.ID does.
type idValue string

func (v idValue) String() string { return string(v) }

// Branch review SFU-9: the call routes log a device as "identity" or "device", a call as "call" and
// a LiveKit room — "<call id>-<seconds>-<random>" — as "room". Each keeps the eight characters every
// *_id keeps, the room its suffix as well, so lines still correlate without the full ids.
func TestCallIdentifiersAreShortenedToo(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger(config.Log{Level: "info", Format: "json"}, &buf)
	full := "0123456789abcdef0123456789abcdef"
	log.Info("call", "identity", full, "device", idValue(full), "call", idValue(full),
		"room", full+"-1790000000-deadbeef")
	log.Info("short", "room", "01234567-1790000000-deadbeef", "device", "x")
	if strings.Contains(buf.String(), full) {
		t.Fatalf("a full identifier reached the log: %s", buf.String())
	}
	var first, second map[string]any
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"identity": "01234567", "device": "01234567", "call": "01234567",
		"room": "01234567-1790000000-deadbeef"} {
		if first[k] != want {
			t.Errorf("%s = %v, want %s", k, first[k], want)
		}
	}
	if second["room"] != "01234567-1790000000-deadbeef" || second["device"] != "x" {
		t.Errorf("an already short room or id changed: %v", second)
	}
}

// secretHolder is the hole a key-based redactor has if it does not resolve:
// slog expands a LogValuer AFTER the handler chain, so an unresolved one walks
// its whole group past the redactor.
type secretHolder struct{}

func (secretHolder) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token", "supersecret"), slog.String("route", "/v1/x"))
}

func TestALogValuerIsResolvedBeforeItIsRedacted(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger(config.Log{Level: "info", Format: "json"}, &buf)
	log.Info("login", slog.Any("session", secretHolder{}))
	if strings.Contains(buf.String(), "supersecret") {
		t.Fatalf("a LogValuer carried a secret past the redactor: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "/v1/x") {
		t.Fatalf("the LogValuer's non-secret attributes were dropped: %s", buf.String())
	}
}

// An E_* refusal code is the natural thing to log and must survive; an invite
// code must not. "code" alone is therefore not a secret key.
func TestARefusalCodeSurvivesTheRedactorButAnInviteCodeDoesNot(t *testing.T) {
	var buf bytes.Buffer
	log := obs.NewLogger(config.Log{Level: "info", Format: "json"}, &buf)
	log.Info("refused",
		slog.String("code", "E_NOT_FOUND"),
		slog.String("invite_code", "ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
	out := buf.String()
	if !strings.Contains(out, "E_NOT_FOUND") {
		t.Fatalf("the refusal code was redacted, making the line useless: %s", out)
	}
	if strings.Contains(out, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Fatalf("an invite code reached the log: %s", out)
	}
}
