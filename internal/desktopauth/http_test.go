package desktopauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHTTPBrowserHandoffRequiresCookieOriginAndPKCE(t *testing.T) {
	b := New(time.Now)
	h := Handler(b, "https://chat.test", nil)
	call := func(method, path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(context.Background(), method, "https://chat.test/desktop-auth/"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	verifier := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	request, _ := json.Marshal(Request{Kind: "get", Challenge: challenge, Options: `{"publicKey":{"challenge":"AAAAAAAAAAAAAAAAAAAAAA"}}`})
	w := call("POST", "request", string(request), "https://evil.test", nil)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = call("POST", "request", string(request), "", nil)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	page := call("GET", created.ID+"/", "", "", nil)
	if page.Code != 200 || !strings.Contains(page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal(page.Code, page.Header())
	}
	cookies := page.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal(cookies)
	}
	if w = call("GET", created.ID+"/options", "", "", nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w = call("POST", created.ID+"/finish", `{"response":"{}"}`, "https://evil.test", cookies[0]); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w = call("POST", created.ID+"/finish", `{"response":"{}"}`, "https://chat.test", cookies[0]); w.Code != 204 {
		t.Fatal(w.Code, w.Body)
	}
	if w = call("POST", created.ID+"/consume", `{"verifier":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`, "", nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = call("POST", created.ID+"/consume", `{"verifier":"`+verifier+`"}`, "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"response":"{}"`) {
		t.Fatal(w.Code, w.Body)
	}
	if strings.Contains(page.Body.String(), verifier) || strings.Contains(page.Body.String(), "response") {
		t.Fatal("credentials in page")
	}
	if w = call("POST", created.ID+"/consume", `{"verifier":"`+verifier+`"}`, "", nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestBrowserProofRemainsScopedUnderAReverseProxyBasePath(t *testing.T) {
	b := New(time.Now)
	id, err := b.Create(Request{Kind: "get", Challenge: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Options: `{"publicKey":{"challenge":"AAAAAAAAAAAAAAAAAAAAAA"}}`})
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.StripPrefix("/team", handler).ServeHTTP(w, r) }))
	defer srv.Close()
	handler = Handler(b, srv.URL, nil)
	client := srv.Client()
	client.Jar, _ = cookiejar.New(nil)
	pageRequest, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/team/desktop-auth/"+id, nil)
	page, err := client.Do(pageRequest)
	if err != nil {
		t.Fatal(err)
	}
	page.Body.Close()
	if page.StatusCode != 200 {
		t.Fatal(page.StatusCode)
	}
	optionsRequest, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/team/desktop-auth/"+id+"/options", nil)
	options, err := client.Do(optionsRequest)
	if err != nil {
		t.Fatal(err)
	}
	options.Body.Close()
	if options.StatusCode != 200 {
		t.Fatalf("browser proof lost under base path: %d", options.StatusCode)
	}
	unrelated, _ := url.Parse(srv.URL + "/other/desktop-auth/" + id + "/options")
	if len(client.Jar.Cookies(unrelated)) != 0 {
		t.Fatal("browser proof escaped its instance base path")
	}
}
