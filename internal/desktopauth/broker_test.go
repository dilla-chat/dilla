package desktopauth

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func TestPKCECompletionIsSingleUseBoundedAndExpires(t *testing.T) {
	now := time.Unix(1800000000, 0)
	b := New(func() time.Time { return now })
	verifier := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	id, err := b.Create(Request{Kind: "get", Challenge: challenge, Options: `{"publicKey":{"challenge":"AAAAAAAAAAAAAAAAAAAAAA"}}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = b.Consume(id, "wrong"); err == nil {
		t.Fatal("wrong verifier accepted")
	}
	if _, ready, err := b.Consume(id, verifier); err != nil || ready {
		t.Fatalf("pending completion: %v %v", ready, err)
	}
	if err = b.Complete(id, `{"id":"credential"}`, ""); err != nil {
		t.Fatal(err)
	}
	result, ready, err := b.Consume(id, verifier)
	if err != nil || !ready || result.Response != `{"id":"credential"}` {
		t.Fatalf("completion: %+v %v %v", result, ready, err)
	}
	if _, _, err = b.Consume(id, verifier); err == nil {
		t.Fatal("completion replay accepted")
	}
	id, err = b.Create(Request{Kind: "get", Challenge: challenge, Options: `{"publicKey":{"challenge":"AAAAAAAAAAAAAAAAAAAAAA"}}`})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	if _, _, err = b.Consume(id, verifier); err == nil {
		t.Fatal("expired completion accepted")
	}
}
func TestBrokerRejectsUnsupportedRequestsAndNeverOverwritesCompletion(t *testing.T) {
	b := New(time.Now)
	for _, r := range []Request{{Kind: "exec", Challenge: "x"}, {Kind: "get", Challenge: "x"}, {Kind: "get", Challenge: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Options: `{}`}} {
		if _, err := b.Create(r); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
}
