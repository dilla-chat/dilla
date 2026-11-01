// The stable Windows launcher derives paths from its executable and Windows known
// folders. Its build configuration is compiled by the desktop packaging script.
package main

import (
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type arguments struct {
	mode            string
	packageManifest string
	forwarded       []string
}

var invitePattern = regexp.MustCompile(`^dilla://invite/([A-Za-z0-9.\[\]:-]+)/([A-Za-z0-9_-]{1,128})$`)

func parseArguments(args []string) (arguments, error) {
	if len(args) == 1 && (args[0] == "--describe" || args[0] == "--state") {
		return arguments{mode: args[0][2:]}, nil
	}
	if len(args) == 1 && (args[0] == "--installer-preflight=initial" || args[0] == "--installer-preflight=uninstall") {
		return arguments{mode: "installer-" + strings.TrimPrefix(args[0], "--installer-preflight=")}, nil
	}
	if len(args) == 2 && args[0] == "--installer-preflight=stage" && strings.HasPrefix(args[1], "--package-manifest-sha512=") {
		digest := strings.TrimPrefix(args[1], "--package-manifest-sha512=")
		raw, err := base64.StdEncoding.Strict().DecodeString(digest)
		if err == nil && len(raw) == 64 && base64.StdEncoding.EncodeToString(raw) == digest {
			return arguments{mode: "installer-stage", packageManifest: digest}, nil
		}
	}
	result := arguments{mode: "launch"}
	hidden, invite := false, false
	for _, arg := range args {
		if arg == "--hidden" && !hidden {
			hidden = true
			result.forwarded = append(result.forwarded, arg)
			continue
		}
		if !invite && validInvite(arg) {
			invite = true
			result.forwarded = append(result.forwarded, arg)
			continue
		}
		return arguments{}, errors.New("unsupported launcher argument")
	}
	return result, nil
}
func validInvite(input string) bool {
	if len(input) > 4096 {
		return false
	}
	match := invitePattern.FindStringSubmatch(input)
	if match == nil {
		return false
	}
	host := match[1]
	u, e := url.Parse("https://" + host + "/")
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" || strings.HasSuffix(u.Hostname(), ".") {
		return false
	}
	if strings.HasPrefix(host, "[") {
		if net.ParseIP(u.Hostname()) == nil || !strings.Contains(u.Hostname(), ":") {
			return false
		}
	} else if strings.Count(host, ":") > 1 || strings.ContainsAny(host, "[]") {
		return false
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 0 || n > 65535 {
			return false
		}
	}
	return true
}
