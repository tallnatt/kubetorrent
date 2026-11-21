package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tallnatt/kubetorrent/portal/internal/auth"
	"github.com/tallnatt/kubetorrent/portal/internal/config"
	"github.com/tallnatt/kubetorrent/portal/internal/k8s"
	"github.com/tallnatt/kubetorrent/portal/internal/proxy"
	"github.com/tallnatt/kubetorrent/portal/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func setupTestServer(t *testing.T, aliceURL, bobURL string) (*Server, *fake.Clientset) {
	t.Helper()
	cfg := &config.Config{
		ListenAddr:        ":0",
		DataDir:           t.TempDir(),
		SessionSecret:     []byte("test-secret-32-bytes-long-enough!"),
		RPID:              "localhost",
		RPOrigins:         []string{"http://localhost:8080"},
		RPDisplayName:     "KubeTorrent Test",
		Namespace:         "kubetorrent",
		GatewayHost:       "kubetorrent-vpn-gateway.kubetorrent.svc.cluster.local",
		HostDownloadsPath: "/srv/kubetorrent/downloads",
		ConfigsPVCName:    "kubetorrent-configs",
		PodGatewayImage:   "ghcr.io/angelnu/pod-gateway:v1.13.0",
		QBittorrentImage:  "lscr.io/linuxserver/qbittorrent:latest",
		NotRoutedCIDRs:    "10.0.0.0/8 172.16.0.0/12 192.168.0.0/16",
		VXLANID:           "42",
		VXLANNetwork:      "172.29.0",
		CookieSecure:      false,
		Users: []config.UserRoute{
			{Username: "admin", InitialPassword: "admin-password-123", IsAdmin: true},
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
	fakeCS := fake.NewSimpleClientset()
	return &Server{
		cfg:         cfg,
		store:       st,
		authMgr:     auth.NewManager(cfg.SessionSecret, cfg.CookieSecure),
		wa:          wa,
		proxy:       proxy.NewRouter(),
		provisioner: k8s.NewFromClient(fakeCS, cfg),
	}, fakeCS
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

	srv, fakeCS := setupTestServer(t, aliceBackend.URL, bobBackend.URL)
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

	// 3. Login as alice -> routes to alice backend, denied /admin
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

	req, _ = http.NewRequest("GET", ts.URL+"/admin/api/users", nil)
	req.AddCookie(aliceCookie)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin accessing /admin/api/users, got %d", res.StatusCode)
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

	// 5. Admin provisions charlie -> creates Deployment and Service in k8s
	adminCookie := loginWithPassword(t, client, ts.URL, "admin", "admin-password-123")
	createBody, _ := json.Marshal(map[string]any{
		"username": "charlie",
		"password": "charlie-password-123",
		"is_admin": false,
	})
	req, _ = http.NewRequest("POST", ts.URL+"/admin/api/users", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating charlie, got %d", res.StatusCode)
	}

	dep, err := fakeCS.AppsV1().Deployments("kubetorrent").Get(context.Background(), "kubetorrent-client-charlie", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected Deployment kubetorrent-client-charlie to be created: %v", err)
	}
	if dep.Spec.Template.Spec.Containers[1].Name != "qbittorrent" {
		t.Fatalf("expected qbittorrent container in charlie deployment")
	}
	if _, err := fakeCS.CoreV1().Services("kubetorrent").Get(context.Background(), "kubetorrent-client-charlie", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected Service kubetorrent-client-charlie to be created: %v", err)
	}

	// 6. Admin deletes charlie -> deletes Deployment and Service
	req, _ = http.NewRequest("DELETE", ts.URL+"/admin/api/users/charlie", nil)
	req.AddCookie(adminCookie)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 deleting charlie, got %d", res.StatusCode)
	}
	if _, err := fakeCS.AppsV1().Deployments("kubetorrent").Get(context.Background(), "kubetorrent-client-charlie", metav1.GetOptions{}); err == nil {
		t.Fatalf("expected Deployment kubetorrent-client-charlie to be deleted")
	}

	// 7. Passkey registration begin works when authenticated
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

	// 8. Discoverable passkey login begin works when unauthenticated
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
