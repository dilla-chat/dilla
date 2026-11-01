package desktoplaunch

import "testing"

// An unrelated owner or inherited writable broad group must not control program content.
func TestProgramSecurityRejectsUntrustedWriteAuthority(t *testing.T) {
	trusted := map[string]bool{"user": true, "token-owner": true, "S-1-5-18": true, "S-1-5-32-544": true}
	for _, test := range []struct {
		name, owner string
		null        bool
		grants      []accessGrant
		valid       bool
	}{
		{"user private", "user", false, []accessGrant{{SID: "user", Mask: 0x1f01ff}}, true},
		{"admin token owner", "token-owner", false, []accessGrant{{SID: "S-1-5-18", Mask: 0x1f01ff}}, true},
		{"readonly users", "user", false, []accessGrant{{SID: "S-1-5-32-545", Mask: 0x1200a9}}, true},
		{"creator owner inheritance", "user", false, []accessGrant{{SID: "S-1-3-0", Mask: 0x1f01ff, InheritOnly: true}}, true},
		{"foreign owner", "foreign", false, nil, false},
		{"null dacl", "user", true, nil, false},
		{"broad write", "user", false, []accessGrant{{SID: "S-1-1-0", Mask: 2}}, false},
		{"foreign delete", "user", false, []accessGrant{{SID: "foreign", Mask: 0x10000}}, false},
		{"inherited broad write", "user", false, []accessGrant{{SID: "S-1-5-11", Mask: 0x40000000, InheritOnly: true}}, false},
		{"foreign owner inheritance", "user", false, []accessGrant{{SID: "S-1-3-0", Mask: 0x1f01ff}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateSecurity(test.owner, test.null, test.grants, trusted)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v err=%v", test.valid, err)
			}
		})
	}
}
