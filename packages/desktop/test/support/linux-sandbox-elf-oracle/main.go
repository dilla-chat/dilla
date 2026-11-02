// Test-only oracle: mirrors the resident verifier's debug/elf checks.
package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"os"
)

func valid(raw []byte) bool {
	if len(raw) < 64 || binary.LittleEndian.Uint16(raw[52:]) != 64 {
		return false
	}
	f, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Version != elf.EV_CURRENT || f.Machine != elf.EM_X86_64 || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return false
	}
	entry := false
	for _, p := range f.Progs {
		if p.Filesz > uint64(len(raw)) || p.Off > uint64(len(raw))-p.Filesz || p.Memsz < p.Filesz {
			return false
		}
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 && p.Filesz > 0 && f.Entry >= p.Vaddr && f.Entry-p.Vaddr < p.Memsz {
			entry = true
		}
	}
	return entry
}
func main() {
	var inputs [][]byte
	if json.NewDecoder(os.Stdin).Decode(&inputs) != nil {
		os.Exit(2)
	}
	out := make([]bool, len(inputs))
	for i, raw := range inputs {
		out[i] = valid(raw)
	}
	if json.NewEncoder(os.Stdout).Encode(out) != nil {
		os.Exit(2)
	}
}
