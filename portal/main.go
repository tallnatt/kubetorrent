package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tallnatt/kubetorrent/portal/internal/auth"
	"github.com/tallnatt/kubetorrent/portal/internal/config"
	"github.com/tallnatt/kubetorrent/portal/internal/k8s"
	"github.com/tallnatt/kubetorrent/portal/internal/proxy"
	"github.com/tallnatt/kubetorrent/portal/internal/store"
	"github.com/tallnatt/kubetorrent/portal/internal/web"
)

type Server struct {
	cfg         *config.Config
	store       *store.Store
	authMgr     *auth.Manager
	wa          *webauthn.WebAuthn
	proxy       *proxy.Router
	provisioner k8s.Provisioner
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	st, err := store.New(cfg.DataDir, cfg.Users)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}

	wa, err := auth.NewWebAuthn(cfg)
	if err != nil {
		log.Fatalf("init webauthn: %v", err)
	}

	prov, err := k8s.NewProvisioner(cfg)
	if err != nil {
		log.Printf("Kubernetes in-cluster client unavailable (%v); using noop provisioner", err)
		prov = k8s.NoopProvisioner{}
	}

	srv := &Server{
		cfg:         cfg,
		store:       st,
		authMgr:     auth.NewManager(cfg.SessionSecret, cfg.CookieSecure),
		wa:          wa,
		proxy:       proxy.NewRouter(),
		provisioner: prov,
	}

	// Ensure configured non-admin users have their Kubernetes Pods & Services provisioned
	for _, u := range st.ListUsers() {
		if !u.IsAdmin && u.BackendURL != "" {
			if err := prov.ProvisionUser(context.Background(), u.Username); err != nil {
				log.Printf("WARN: failed to reconcile resources for %s: %v", u.Username, err)
			}
		}
	}

	mux := srv.routes()
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("KubeTorrent Portal listening on %s (RPID=%s, namespace=%s)", cfg.ListenAddr, cfg.RPID, cfg.Namespace)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /portal/styles.css", func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(web.Assets, "styles.css")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /account", s.handleAccountPage)
	mux.HandleFunc("GET /admin", s.handleAdminPage)

	mux.HandleFunc("POST /auth/login", s.handlePasswordLogin)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /auth/me", s.handleMe)
	mux.HandleFunc("POST /auth/password", s.handlePasswordUpdate)

	mux.HandleFunc("POST /auth/passkey/register/begin", s.handlePasskeyRegisterBegin)
	mux.HandleFunc("POST /auth/passkey/register/finish", s.handlePasskeyRegisterFinish)
	mux.HandleFunc("POST /auth/passkey/delete", s.handlePasskeyDelete)

	mux.HandleFunc("POST /auth/passkey/login/begin", s.handlePasskeyLoginBegin)
	mux.HandleFunc("POST /auth/passkey/login/finish", s.handlePasskeyLoginFinish)

	mux.HandleFunc("GET /admin/api/users", s.handleAdminListUsers)
	mux.HandleFunc("POST /admin/api/users", s.handleAdminCreateUser)
	mux.HandleFunc("DELETE /admin/api/users/{username}", s.handleAdminDeleteUser)
	mux.HandleFunc("POST /admin/api/users/{username}/password", s.handleAdminResetPassword)
	mux.HandleFunc("POST /admin/api/users/{username}/passkeys/revoke", s.handleAdminRevokePasskeys)

	mux.HandleFunc("/", s.handleProxy)
	return mux
}

func (s *Server) requireAdmin(r *http.Request) (*store.User, int, string) {
	username, err := s.authMgr.GetSessionUser(r)
	if err != nil {
		return nil, http.StatusUnauthorized, "unauthorized"
	}
	user, err := s.store.GetByUsername(username)
	if err != nil {
		return nil, http.StatusUnauthorized, "user not found"
	}
	if !user.IsAdmin {
		return nil, http.StatusForbidden, "admin access required"
	}
	return user, http.StatusOK, ""
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authMgr.GetSessionUser(r); err == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	data, err := fs.ReadFile(web.Assets, "login.html")
	if err != nil {
		http.Error(w, "missing login page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authMgr.GetSessionUser(r); err != nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	data, err := fs.ReadFile(web.Assets, "account.html")
	if err != nil {
		http.Error(w, "missing account page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	_, status, _ := s.requireAdmin(r)
	if status == http.StatusUnauthorized {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if status == http.StatusForbidden {
		http.Error(w, "forbidden: admin access required", http.StatusForbidden)
		return
	}
	data, err := fs.ReadFile(web.Assets, "admin.html")
	if err != nil {
		http.Error(w, "missing admin page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	user, err := s.store.VerifyPassword(req.Username, req.Password)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if err := s.authMgr.SetSessionCookie(w, user.Username); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	redirect := "/"
	if user.IsAdmin && user.BackendURL == "" {
		redirect = "/admin"
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.authMgr.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	username, err := s.authMgr.GetSessionUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	user, err := s.store.GetByUsername(username)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "user not found")
		return
	}
	type passkeyView struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		CreatedAt time.Time `json:"created_at"`
	}
	pks := make([]passkeyView, 0, len(user.Passkeys))
	for _, pk := range user.Passkeys {
		pks = append(pks, passkeyView{
			ID:        base64.RawURLEncoding.EncodeToString(pk.Credential.ID),
			Name:      pk.Name,
			CreatedAt: pk.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": user.Username,
		"is_admin": user.IsAdmin,
		"passkeys": pks,
	})
}

func (s *Server) handlePasswordUpdate(w http.ResponseWriter, r *http.Request) {
	username, err := s.authMgr.GetSessionUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.UpdatePassword(username, req.Password); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	username, err := s.authMgr.GetSessionUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	user, err := s.store.GetByUsername(username)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	options, sessionData, err := s.wa.BeginRegistration(user)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.authMgr.SaveChallenge(w, user.Username, sessionData); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, options)
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	username, err := s.authMgr.GetSessionUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	entry, err := s.authMgr.PopChallenge(w, r)
	if err != nil || entry.Username != username {
		writeJSONError(w, http.StatusBadRequest, "invalid or expired challenge")
		return
	}
	user, err := s.store.GetByUsername(username)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	cred, err := s.wa.FinishRegistration(user, *entry.Data, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "passkey registration failed: "+err.Error())
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if err := s.store.AddPasskey(username, name, *cred); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}

func (s *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	username, err := s.authMgr.GetSessionUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	credID, err := base64.RawURLEncoding.DecodeString(req.ID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid passkey ID")
		return
	}
	if err := s.store.DeletePasskey(username, credID); err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	req.Username = strings.TrimSpace(req.Username)

	if req.Username != "" {
		user, err := s.store.GetByUsername(req.Username)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "user not found")
			return
		}
		if len(user.Passkeys) == 0 {
			writeJSONError(w, http.StatusBadRequest, "no passkeys registered for this user")
			return
		}
		options, sessionData, err := s.wa.BeginLogin(user)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.authMgr.SaveChallenge(w, user.Username, sessionData); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, options)
		return
	}

	options, sessionData, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.authMgr.SaveChallenge(w, "", sessionData); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, options)
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	entry, err := s.authMgr.PopChallenge(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid or expired challenge")
		return
	}

	var loggedInUser *store.User
	if entry.Username != "" {
		user, err := s.store.GetByUsername(entry.Username)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "user not found")
			return
		}
		cred, err := s.wa.FinishLogin(user, *entry.Data, r)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "passkey verification failed: "+err.Error())
			return
		}
		_ = s.store.UpdatePasskey(user.Username, *cred)
		loggedInUser = user
	} else {
		var matchedUser *store.User
		cred, err := s.wa.FinishDiscoverableLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
			u, err := s.store.GetByID(userHandle)
			if err != nil {
				return nil, err
			}
			matchedUser = u
			return u, nil
		}, *entry.Data, r)
		if err != nil || matchedUser == nil {
			writeJSONError(w, http.StatusUnauthorized, "passkey verification failed")
			return
		}
		_ = s.store.UpdatePasskey(matchedUser.Username, *cred)
		loggedInUser = matchedUser
	}

	if err := s.authMgr.SetSessionCookie(w, loggedInUser.Username); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	redirect := "/"
	if loggedInUser.IsAdmin && loggedInUser.BackendURL == "" {
		redirect = "/admin"
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
}

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	adminUser, status, msg := s.requireAdmin(r)
	if status != http.StatusOK {
		writeJSONError(w, status, msg)
		return
	}

	type userView struct {
		Username     string `json:"username"`
		IsAdmin      bool   `json:"is_admin"`
		IsSelf       bool   `json:"is_self"`
		PasskeyCount int    `json:"passkey_count"`
		PodStatus    string `json:"pod_status,omitempty"`
	}

	users := s.store.ListUsers()
	views := make([]userView, 0, len(users))
	for _, u := range users {
		podStatus := ""
		if !u.IsAdmin {
			podStatus = s.provisioner.GetUserStatus(r.Context(), u.Username)
		}
		views = append(views, userView{
			Username:     u.Username,
			IsAdmin:      u.IsAdmin,
			IsSelf:       u.Username == adminUser.Username,
			PasskeyCount: len(u.Passkeys),
			PodStatus:    podStatus,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": views})
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	_, status, msg := s.requireAdmin(r)
	if status != http.StatusOK {
		writeJSONError(w, status, msg)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Username = strings.TrimSpace(req.Username)

	backendURL := ""
	if !req.IsAdmin {
		backendURL = s.cfg.DefaultUserBackendURL(req.Username, 8080)
	}
	if _, err := s.store.CreateUser(req.Username, req.Password, backendURL, req.IsAdmin); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrUserExists) {
			code = http.StatusConflict
		}
		writeJSONError(w, code, err.Error())
		return
	}

	if !req.IsAdmin {
		if err := s.provisioner.ProvisionUser(r.Context(), req.Username); err != nil {
			_ = s.store.DeleteUser(req.Username)
			writeJSONError(w, http.StatusInternalServerError, "failed to provision Kubernetes Pod: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]string{"username": req.Username})
}

func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	adminUser, status, msg := s.requireAdmin(r)
	if status != http.StatusOK {
		writeJSONError(w, status, msg)
		return
	}
	target := r.PathValue("username")
	if target == adminUser.Username {
		writeJSONError(w, http.StatusBadRequest, "cannot delete your own admin account")
		return
	}
	u, err := s.store.GetByUsername(target)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	if !u.IsAdmin {
		if err := s.provisioner.DeprovisionUser(r.Context(), target); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to delete Kubernetes Pod: "+err.Error())
			return
		}
	}
	if err := s.store.DeleteUser(target); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	_, status, msg := s.requireAdmin(r)
	if status != http.StatusOK {
		writeJSONError(w, status, msg)
		return
	}
	target := r.PathValue("username")
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.UpdatePassword(target, req.Password); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleAdminRevokePasskeys(w http.ResponseWriter, r *http.Request) {
	_, status, msg := s.requireAdmin(r)
	if status != http.StatusOK {
		writeJSONError(w, status, msg)
		return
	}
	target := r.PathValue("username")
	if err := s.store.RevokePasskeys(target); err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	username, err := s.authMgr.GetSessionUser(r)
	if err != nil {
		if strings.Contains(r.Header.Get("Accept"), "text/html") || r.URL.Path == "/" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := s.store.GetByUsername(username)
	if err != nil {
		s.authMgr.ClearSessionCookie(w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if user.IsAdmin && user.BackendURL == "" {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	s.proxy.ServeUser(w, r, user.BackendURL)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
