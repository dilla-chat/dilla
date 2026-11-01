package desktoplaunch

import (
	"strings"
	"testing"
)

func TestPendingRecordsCannotSelectArbitraryRecovery(t *testing.T) {
	nonce := strings.Repeat("a", 64)
	if n, e := ParseActiveRecord([]byte(`{"format":1,"nonce":"` + nonce + `"}`)); e != nil || n != nonce {
		t.Fatalf("active %q %v", n, e)
	}
	if j, e := ParseJournal([]byte(`{"format":1,"nonce":"`+nonce+`","phase":"complete","childPid":123,"version":"1.2.3"}`), nonce); e != nil || j.Version != "1.2.3" {
		t.Fatalf("journal %+v %v", j, e)
	}
	for _, raw := range []string{`{"format":1,"nonce":"../evil"}`, `{"format":1,"nonce":"` + nonce + `","command":"evil"}`, `{"format":2,"format":1,"nonce":"` + nonce + `"}`} {
		if _, e := ParseActiveRecord([]byte(raw)); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"format":1,"nonce":"` + nonce + `","phase":"complete"}`, `{"format":1,"nonce":"` + nonce + `","phase":"failed","childPid":1}`, `{"format":1,"nonce":"` + nonce + `","phase":"failed","command":"evil"}`, `{"format":1,"nonce":"` + nonce + `","phase":"failed","version":"1.2.3"}`, `{"format":1,"nonce":"` + nonce + `","phase":"other"}`} {
		if _, e := ParseJournal([]byte(raw), nonce); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, fields := range []string{`"phase":"complete","childPid":123,"version":"0.0.0-beta.1"`, `"phase":"failed","failure":""`} {
		if _, e := ParseJournal([]byte(`{"format":1,"nonce":"`+nonce+`",`+fields+`}`), nonce); e == nil {
			t.Fatalf("accepted invalid journal fields %s", fields)
		}
	}
}
