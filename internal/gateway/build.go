package gateway

import (
	"regexp"
	"strings"

	"github.com/dilla-chat/dilla/internal/auth"
)

var appEntryPattern = regexp.MustCompile(`^/assets/[A-Za-z0-9_./-]+\.js$`)

func validAppEntry(entry string) bool {
	return len(entry) <= 512 && !strings.Contains(entry, "..") && appEntryPattern.MatchString(entry)
}
func (g *Gateway) sendAppBuild(c *conn) {
	// app_build tells a web client to update; a managed Bot has no web client, so none is sent to its connections.
	if c.isBot || c.wire < auth.WireVersion || g.opts.CurrentAppEntry == nil {
		return
	}
	entry := g.opts.CurrentAppEntry()
	if !validAppEntry(entry) {
		return
	}
	p, err := payload(entry)
	if err != nil {
		return
	}
	c.send(Frame{Op: OpAppBuild, Payload: p})
}
