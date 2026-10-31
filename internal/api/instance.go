package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/jonasthim/dilla/internal/server"
)

// registerInstance mounts the two discovery routes. GET /v1/instance is
// unauthenticated unless instance.discovery = "session", which is the one knob
// protocol/09-http-api.md § Instance puts on that row's Auth column; the limits
// array has no such knob and is always open, because a client needs
// max_ciphertext_bytes before it has a session to ask with.
func registerInstance(m *server.Mux, d Deps) {
	if d.Config != nil && d.Config.Instance.Discovery == "session" {
		// `E, F`: a client setting up a factor reads auth_methods from here (web3.6).
		m.Handle("GET /v1/instance", d.owedOK(d.InstanceDocument))
	} else {
		m.Handle("GET /v1/instance", http.HandlerFunc(d.InstanceDocument))
	}
	m.Handle("GET /v1/instance/limits", http.HandlerFunc(d.InstanceLimits))
}

// InstanceDocument serves GET /v1/instance: the eleven-element discovery document of
// protocol/09-http-api.md § Instance. It is unauthenticated unless
// instance.discovery = "session"; Register decides which by wrapping this
// handler. registration_mode is 0 invite-only, 1 open, 2 closed; auth_methods
// are 0 password, 1 totp, 2 passkey, 3 oidc — both enumerations are fixed by
// that section, including the meaning of 0.
//
// It is NOT named Instance: Deps already carries the instance ROW under that
// name, and a struct cannot hold a field and a method with one identifier.
func (d Deps) InstanceDocument(w http.ResponseWriter, r *http.Request) {
	// Fail CLOSED on a missing Config, the way DELETE /v1/accounts/me does:
	// registerInstance itself admits a nil Config (it decides the route's auth
	// from it), so this handler can be reached without one, and both the
	// domain and the whole auth_methods enumeration come from it. Publishing a
	// discovery document with an empty domain and no methods would be worse
	// than refusing, and dereferencing it is a panic that only production's
	// server.Recover turns into a 500.
	if d.Config == nil {
		d.logf(r, "api: instance route without a config")
		server.WriteError(w, server.Errorf(server.CodeInternal, ""))
		return
	}
	// The same for the external-sender key: a document without it would send every client that
	// creates a text group off to build one the instance can propose nothing into.
	if len(d.ExternalSenderPub) != 32 {
		d.logf(r, "api: instance route without the external-sender key")
		server.WriteError(w, server.Errorf(server.CodeInternal, ""))
		return
	}
	methods := make([]uint64, 0, len(d.Config.Auth.Methods))
	for _, m := range d.Config.Auth.Methods {
		switch m {
		case "password":
			methods = append(methods, 0)
		case "totp":
			methods = append(methods, 1)
		case "passkey":
			methods = append(methods, 2)
		case "oidc":
			methods = append(methods, 3)
		}
	}
	// "ascending, no duplicates" is the document's wording, and config.Validate
	// constrains neither the order an operator writes auth.methods in nor a
	// repeated entry.
	slices.Sort(methods)
	methods = slices.Compact(methods)
	mode := uint64(0)
	switch d.Registration.Mode {
	case "open":
		mode = 1
	case "closed":
		mode = 2
	}
	// The hello's own list (web3.8 S1): the composition root passes the one it hands the gateway.
	wire := d.WireVersions
	if len(wire) == 0 {
		// Match dillad's production offer when a focused handler test omits composition wiring.
		wire = []uint64{5, 4, 3, 2}
	}
	e2ee := d.E2EEVersions
	if len(e2ee) == 0 {
		e2ee = []uint64{1}
	}
	media := d.MediaVersions
	if len(media) == 0 {
		media = []uint64{1}
	}
	doc := []any{
		wire,  // wire_versions
		e2ee,  // e2ee_versions
		media, // media_versions
		d.Instance.InstanceID,
		d.Instance.Generation,
		d.Config.Instance.Domain,
		mode,
		methods,
		d.Instance.PolicyVersion,
		d.Instance.ExternalSenderKeyID, // external_sender_key_id
		d.ExternalSenderPub,            // external_sender_pub
	}
	d.generation(w)
	if err := server.EncodeBody(w, http.StatusOK, doc); err != nil {
		d.logf(r, "api: encode instance document", "err", err)
	}
}

// InstanceLimits serves GET /v1/instance/limits: eleven unsigned integers, in
// the order protocol/09-http-api.md § Instance fixes.
func (d Deps) InstanceLimits(w http.ResponseWriter, r *http.Request) {
	// Every one of the eleven numbers is a config value; see InstanceDocument.
	if d.Config == nil {
		d.logf(r, "api: instance route without a config")
		server.WriteError(w, server.Errorf(server.CodeInternal, ""))
		return
	}
	c := d.Config
	limits := []uint64{
		uint64(c.Limits.MaxCiphertextBytes),      //nolint:gosec // G115: a config value that Validate keeps positive
		uint64(c.Blobs.MaxBlobBytes),             //nolint:gosec // G115: a config value that Validate keeps positive
		4,                                        // max_attachments (protocol/04, tightened by R6/R32)
		2,                                        // max_previews
		uint64(c.Limits.MaxKeypackagesPerDevice), //nolint:gosec // G115: a config value that Validate keeps positive
		uint64(c.Limits.KeypackageRefillThreshold),                     //nolint:gosec // G115: a config value that Validate keeps positive
		uint64(c.Blobs.QuotaBytesPerUser),                              //nolint:gosec // G115: a config value that Validate keeps positive
		uint64(c.Gateway.HeartbeatInterval.Value() / time.Millisecond), //nolint:gosec // G115: a non-negative duration in milliseconds
		// max_frame_bytes is derived once, in config.Derive, and part 1b's
		// gateway.Options.MaxFrameBytes reads the same accessor: an operator who
		// changes limits.max_ciphertext_bytes must move both this element and
		// the gateway's hello frame, not one of them.
		c.MaxFrameBytes(),
		uint64(c.Retention.HandshakeDays),  //nolint:gosec // G115: a config value that Validate keeps positive
		uint64(c.Retention.CiphertextDays), //nolint:gosec // G115: a config value that Validate keeps positive
	}
	d.generation(w)
	if err := server.EncodeBody(w, http.StatusOK, limits); err != nil {
		d.logf(r, "api: encode limits document", "err", err)
	}
}
