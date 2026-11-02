// Package desktopauth carries a short-lived browser ceremony back to its desktop
// initiator. PKCE S256 binds consumption; neither credentials nor verifiers appear in URLs.
package desktopauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"time"
)

var opaque = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var verifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
var ErrInvalid = errors.New("desktop browser ceremony unavailable")

type Request struct {
	Kind      string `json:"kind"`
	Challenge string `json:"challenge"`
	Options   string `json:"options"`
}
type Result struct {
	Response  string `json:"response,omitempty"`
	Assertion string `json:"assertion,omitempty"`
	Needs     uint8  `json:"needs"`
	Error     string `json:"error,omitempty"`
}
type row struct {
	Request
	Result
	ready       bool
	browser     string
	oidcTicket  string
	oidcClaimed bool
	expires     time.Time
}
type Broker struct {
	mu   sync.Mutex
	now  func() time.Time
	rows map[string]*row
}

func New(now func() time.Time) *Broker { return &Broker{now: now, rows: make(map[string]*row)} }
func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (b *Broker) find(id string) (*row, error) {
	r := b.rows[id]
	if !opaque.MatchString(id) || r == nil || !b.now().Before(r.expires) {
		delete(b.rows, id)
		return nil, ErrInvalid
	}
	return r, nil
}
func (b *Broker) Create(request Request) (string, error) {
	if !opaque.MatchString(request.Challenge) || (request.Kind != "get" && request.Kind != "create" && request.Kind != "oidc") || len(request.Options) > 65536 || (request.Kind == "oidc" && request.Options != "") {
		return "", ErrInvalid
	}
	if value, err := base64.RawURLEncoding.DecodeString(request.Challenge); err != nil || len(value) != 32 {
		return "", ErrInvalid
	}
	if request.Kind != "oidc" {
		var options struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		}
		if json.Unmarshal([]byte(request.Options), &options) != nil {
			return "", ErrInvalid
		}
		challenge, err := base64.RawURLEncoding.DecodeString(options.PublicKey.Challenge)
		if err != nil || len(challenge) < 16 || len(challenge) > 1024 {
			return "", ErrInvalid
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, r := range b.rows {
		if !b.now().Before(r.expires) {
			delete(b.rows, id)
		}
	}
	if len(b.rows) >= 512 {
		return "", ErrInvalid
	}
	id := token()
	b.rows[id] = &row{Request: request, expires: b.now().Add(2 * time.Minute)}
	return id, nil
}
func (b *Broker) Browser(id string) (Request, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.find(id)
	if err != nil {
		return Request{}, "", err
	}
	if r.browser == "" {
		r.browser = token()
	}
	return r.Request, r.browser, nil
}
func (b *Broker) VerifyBrowser(id, value string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.find(id)
	return err == nil && r.browser != "" && subtle.ConstantTimeCompare([]byte(r.browser), []byte(value)) == 1
}
func (b *Broker) Complete(id, response, problem string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.find(id)
	if err != nil || r.ready || r.Kind == "oidc" || len(response) > 65536 || len(problem) > 128 {
		return ErrInvalid
	}
	if response != "" && !json.Valid([]byte(response)) {
		return ErrInvalid
	}
	if response == "" && problem == "" {
		return ErrInvalid
	}
	r.Result = Result{Response: response, Error: problem}
	r.ready = true
	return nil
}
func (b *Broker) IsOIDC(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.find(id)
	return err == nil && r.Kind == "oidc" && !r.ready
}
func (b *Broker) CompleteOIDC(id, assertion string, needs uint8) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.find(id)
	if err != nil || r.Kind != "oidc" || !r.oidcClaimed || r.ready || !opaque.MatchString(assertion) || needs > 2 {
		return ErrInvalid
	}
	r.Result = Result{Assertion: assertion, Needs: needs}
	r.ready = true
	return nil
}
func (b *Broker) Consume(id, verifier string) (Result, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.find(id)
	if err != nil || !verifierPattern.MatchString(verifier) {
		return Result{}, false, ErrInvalid
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(challenge), []byte(r.Challenge)) != 1 {
		return Result{}, false, ErrInvalid
	}
	if !r.ready {
		return Result{}, false, nil
	}
	delete(b.rows, id)
	return r.Result, true, nil
}

// OIDCTicket grants only the next browser redirect. It carries no assertion and
// cannot complete or consume the desktop request.
func (b *Broker) OIDCTicket(id string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.find(id)
	if err != nil || r.Kind != "oidc" || r.ready || r.oidcClaimed {
		return "", ErrInvalid
	}
	if r.oidcTicket == "" {
		r.oidcTicket = token()
	}
	return r.oidcTicket, nil
}
func (b *Broker) ClaimOIDC(id, value string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.find(id)
	if err != nil || r.Kind != "oidc" || r.ready || r.oidcClaimed || r.oidcTicket == "" || subtle.ConstantTimeCompare([]byte(r.oidcTicket), []byte(value)) != 1 {
		return false
	}
	r.oidcTicket = ""
	r.oidcClaimed = true
	return true
}
