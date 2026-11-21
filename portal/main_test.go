package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tallnatt/kubetorrent/portal/internal/auth"
	"github.com/tallnatt/kubetorrent/portal/internal/config"
	"github.com/tallnatt/kubetorrent/portal/internal/proxy"
	"github.com/tallnatt/kubetorrent/portal/internal/store"
)

func setupTestServer(t *testing.T, aliceURL, bobURL string) *Server {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:    ":0",
		DataDir:       t.TempDir(),
		SessionSecret: []byte("test-secret-32-bytes-long-enough!"),
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost:8080"},
		RPDisplayName: "KubeTorrent Test",
		CookieSecure:  false,
		Users: []config.UserRoute{
			{Username: "alice", BackendURL: aliceURL, InitialPassword: "alice-password-123"},
			{Username: "bob", BackendURL: bobURL, InitialPassword: "bob-password-123"},
		},
	}
	st, err := store.New(cfg.DataDir, cfg.Users)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	wa, err := auth.NewWebAuthn(cfg)
	if err != nil {
		t.Fatalf("auth.NewWebAuthn: %v", err)
	}
	return &Server{
		cfg:     cfg,
		store:   st,
		authMgr: auth.NewManager(cfg.SessionSecret, cfg.CookieSecure),
		wa:      wa,
		proxy:   proxy.NewRouter(),
	}
}

func TestPortalAuthAndUserSegregation(t *testing.T) {
	aliceBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("backend:alice:" + r.URL.Path))
	}))
	defer aliceBackend.Close()

	bobBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("backend:bob:" + r.URL.Path))
	}))
	defer bobBackend.Close()

	srv := setupTestServer(t, aliceBackend.URL, bobBackend.URL)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 1. Unauthenticated / redirects to /login
	res, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login, got %d (%s)", res.StatusCode, res.Header.Get("Location"))
	}

	// 2. Wrong password returns 401
	badBody, _ := json.Marshal(map[string]string{"username": "alice", "password": "wrong"})
	res, err = client.Post(ts.URL+"/auth/login", "application/json", bytes.NewReader(badBody))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d", res.StatusCode)
	}

	// 3. Login as alice -> routes to alice backend
	aliceCookie := loginWithPassword(t, client, ts.URL, "alice", "alice-password-123")
	req, _ := http.NewRequest("GET", ts.URL+"/api/v2/app/version", nil)
	req.AddCookie(aliceCookie)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var buf [128]byte
	n, _ := res.Body.Read(buf[:])
	res.Body.Close()
	if got := string(buf[:n]); got != "backend:alice:/api/v2/app/version" {
		t.Fatalf("expected alice backend, got %q", got)
	}

	// 4. Login as bob -> routes to bob backend
	bobCookie := loginWithPassword(t, client, ts.URL, "bob", "bob-password-123")
	req, _ = http.NewRequest("GET", ts.URL+"/api/v2/app/version", nil)
	req.AddCookie(bobCookie)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, _ = res.Body.Read(buf[:])
	res.Body.Close()
	if got := string(buf[:n]); got != "backend:bob:/api/v2/app/version" {
		t.Fatalf("expected bob backend, got %q", got)
	}

	// 5. Passkey registration begin works when authenticated
	req, _ = http.NewRequest("POST", ts.URL+"/auth/passkey/register/begin", nil)
	req.AddCookie(aliceCookie)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on passkey register begin, got %d", res.StatusCode)
	}

	// 6. Discoverable passkey login begin works when unauthenticated
	res, err = client.Post(ts.URL+"/auth/passkey/login/begin", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on discoverable passkey login begin, got %d", res.StatusCode)
	}
}

func loginWithPassword(t *testing.T, client *http.Client, baseURL, user, pass string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	res, err := client.Post(baseURL+"/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login failed for %s: status %d", user, res.StatusCode)
	}
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	t.Fatalf("session cookie not returned for %s", user)
	return nil
}
