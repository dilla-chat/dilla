package exit_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/exit"
)

func TestCodeValues(t *testing.T) {
	for name, pair := range map[string]struct {
		got  exit.Code
		want int
	}{
		"OK": {exit.OK, 0}, "Fail": {exit.Fail, 1}, "Usage": {exit.Usage, 2},
		"NotImplemented": {exit.NotImplemented, 3}, "Data": {exit.Data, 65},
		"Unavailable": {exit.Unavailable, 69}, "Software": {exit.Software, 70},
		"CantCreate": {exit.CantCreate, 73}, "IOErr": {exit.IOErr, 74},
		"TempFail": {exit.TempFail, 75}, "NoPerm": {exit.NoPerm, 77}, "Config": {exit.Config, 78},
	} {
		t.Run(name, func(t *testing.T) {
			if int(pair.got) != pair.want {
				t.Fatalf("%s = %d, want %d", name, int(pair.got), pair.want)
			}
		})
	}
}

func TestCodeUnwrapsThroughWrapping(t *testing.T) {
	err := fmt.Errorf("loading config: %w", exit.Config)
	var code exit.Code
	if !errors.As(err, &code) {
		t.Fatal("errors.As did not find the code")
	}
	if code != exit.Config {
		t.Fatalf("code = %d, want 78", int(code))
	}
}
