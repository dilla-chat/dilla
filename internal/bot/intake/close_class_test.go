package intake

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
)

func loggedWorld(t *testing.T) (*world, *bytes.Buffer) {
	w := newWorld(t)
	var out bytes.Buffer
	w.in.SetLogger(slog.New(slog.NewTextHandler(&out, nil)))
	return w, &out
}

func TestAnswerNotSealedLogsClass(t *testing.T) {
	w, out := loggedWorld(t)
	w.helper.sealErr = helper.ErrClosed
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	line := out.String()
	for _, want := range []string{"bot command answer not sealed", "error_class=closed", "op=", "seq=0"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
}

func TestOpenFailedLogsClass(t *testing.T) {
	w, out := loggedWorld(t)
	w.helper.err = helper.ErrClosed
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	line := out.String()
	if !strings.Contains(line, "bot command open failed") || !strings.Contains(line, "error_class=closed") || strings.Contains(line, "helper_error") {
		t.Fatalf("log %q", line)
	}
	w2, out2 := loggedWorld(t)
	w2.helper.err = errors.New("something else")
	cmd, _ = w2.command(opRemind, 1)
	w2.in.Handle(context.Background(), cmd)
	if !strings.Contains(out2.String(), "error_class=other") {
		t.Fatalf("log %q", out2.String())
	}
}
