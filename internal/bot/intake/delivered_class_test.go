package intake

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAnswerNotDeliveredLogsClassNotText(t *testing.T) {
	w, out := loggedWorld(t)
	w.sender.err = errors.New("secret-reply-failure-text")
	cmd, _ := w.command(opRemind, 1)
	w.in.Handle(context.Background(), cmd)
	line := out.String()
	if !strings.Contains(line, "bot command answer not delivered") || !strings.Contains(line, "error_class=other") {
		t.Fatalf("log %q", line)
	}
	if strings.Contains(line, "secret-reply-failure-text") || strings.Contains(line, "reply_error") {
		t.Fatalf("error text or old class leaked: %q", line)
	}
}
