package desktoplaunch

import "errors"

type accessGrant struct {
	SID         string
	Mask        uint32
	InheritOnly bool
}

func validateSecurity(owner string, nullDACL bool, grants []accessGrant, trusted map[string]bool) error {
	if !trusted[owner] {
		return errors.New("program entry has an untrusted owner")
	}
	if nullDACL {
		return errors.New("program entry has a null DACL")
	}
	// GENERIC_ALL/WRITE, DELETE, WRITE_DAC/OWNER, FILE_WRITE_DATA,
	// APPEND_DATA, WRITE_EA, DELETE_CHILD and WRITE_ATTRIBUTES.
	const mutationRights uint32 = 0x10000000 | 0x40000000 | 0x10000 | 0x40000 | 0x80000 | 0x2 | 0x4 | 0x10 | 0x40 | 0x100
	for _, g := range grants {
		if trusted[g.SID] || g.InheritOnly && g.SID == "S-1-3-0" {
			continue
		}
		if g.Mask&mutationRights != 0 {
			return errors.New("program entry grants mutation rights to an untrusted principal")
		}
	}
	return nil
}
