package native

import "testing"

func TestInstallationClaimsArePerKeyAndReleaseOnce(t *testing.T) {
	c := newFakeClient(t, &fakeTransport{})
	key := testID(7)
	release, ok := c.TryClaimSource(key)
	if !ok {
		t.Fatal("first source claim refused")
	}
	if _, ok := c.TryClaimSource(key); ok {
		t.Fatal("overlapping source claim admitted")
	}
	other, ok := c.TryClaimSource(testID(8))
	if !ok {
		t.Fatal("unrelated source was blocked")
	}
	other()
	trust, ok := c.TryClaimTrust(key)
	if !ok {
		t.Fatal("trust and source claims share a namespace")
	}
	trust()
	release()
	next, ok := c.TryClaimSource(key)
	if !ok {
		t.Fatal("released source remained claimed")
	}
	release() // a stale release must not remove the newer claim.
	if _, ok := c.TryClaimSource(key); ok {
		t.Fatal("double release removed another owner's claim")
	}
	next()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.TryClaimTrust(key); ok {
		t.Fatal("closed client admitted a new claim")
	}
}
