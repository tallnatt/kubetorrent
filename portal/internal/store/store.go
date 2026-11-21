package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tallnatt/kubetorrent/portal/internal/config"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrUserNotFound      = errors.New("user not found")
	ErrUserExists        = errors.New("user already exists")
	ErrInvalidCredential = errors.New("invalid username or password")
	validUsernameRe      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
)

type StoredCredential struct {
	Name       string              `json:"name"`
	CreatedAt  time.Time           `json:"created_at"`
	Credential webauthn.Credential `json:"credential"`
}

type User struct {
	ID           []byte             `json:"id"`
	Username     string             `json:"username"`
	DisplayName  string             `json:"display_name"`
	PasswordHash string             `json:"password_hash"`
	BackendURL   string             `json:"backend_url"`
	IsAdmin      bool               `json:"is_admin,omitempty"`
	Passkeys     []StoredCredential `json:"passkeys"`
}

func (u *User) WebAuthnID() []byte {
	return u.ID
}

func (u *User) WebAuthnName() string {
	return u.Username
}

func (u *User) WebAuthnDisplayName() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

func (u *User) WebAuthnCredentials() []webauthn.Credential {
	creds := make([]webauthn.Credential, len(u.Passkeys))
	for i, pk := range u.Passkeys {
		creds[i] = pk.Credential
	}
	return creds
}

type Store struct {
	mu       sync.RWMutex
	filePath string
	users    map[string]*User // keyed by username
}

func New(dataDir string, routes []config.UserRoute) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	s := &Store{
		filePath: filepath.Join(dataDir, "users.json"),
		users:    make(map[string]*User),
	}

	if err := s.load(); err != nil {
		return nil, err
	}

	if err := s.syncRoutes(routes); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.filePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read users store: %w", err)
	}
	var list []*User
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("parse users store: %w", err)
	}
	for _, u := range list {
		s.users[u.Username] = u
	}
	return nil
}

func (s *Store) saveLocked() error {
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal users store: %w", err)
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp users store: %w", err)
	}
	if err := os.Rename(tmp, s.filePath); err != nil {
		return fmt.Errorf("rename users store: %w", err)
	}
	return nil
}

func (s *Store) syncRoutes(routes []config.UserRoute) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	for _, r := range routes {
		if r.Username == "" {
			continue
		}
		existing, ok := s.users[r.Username]
		if !ok {
			id := make([]byte, 16)
			if _, err := rand.Read(id); err != nil {
				return fmt.Errorf("generate user id: %w", err)
			}
			var hash string
			if r.InitialPassword != "" {
				h, err := bcrypt.GenerateFromPassword([]byte(r.InitialPassword), bcrypt.DefaultCost)
				if err != nil {
					return fmt.Errorf("hash initial password for %s: %w", r.Username, err)
				}
				hash = string(h)
			}
			s.users[r.Username] = &User{
				ID:           id,
				Username:     r.Username,
				DisplayName:  r.Username,
				PasswordHash: hash,
				BackendURL:   r.BackendURL,
				IsAdmin:      r.IsAdmin,
			}
			changed = true
		} else {
			if r.BackendURL != "" && existing.BackendURL != r.BackendURL {
				existing.BackendURL = r.BackendURL
				changed = true
			}
			if r.IsAdmin && !existing.IsAdmin {
				existing.IsAdmin = true
				changed = true
			}
		}
	}

	if changed {
		return s.saveLocked()
	}
	return nil
}

func (s *Store) ListUsers() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, cloneUser(u))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Username < out[j].Username
	})
	return out
}

func (s *Store) CreateUser(username, password, backendURL string, isAdmin bool) (*User, error) {
	if !validUsernameRe.MatchString(username) {
		return nil, errors.New("username must be 1-32 lowercase alphanumeric characters or hyphens")
	}
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[username]; exists {
		return nil, ErrUserExists
	}
	u := &User{
		ID:           id,
		Username:     username,
		DisplayName:  username,
		PasswordHash: string(hash),
		BackendURL:   backendURL,
		IsAdmin:      isAdmin,
	}
	s.users[username] = u
	if err := s.saveLocked(); err != nil {
		delete(s.users, username)
		return nil, err
	}
	return cloneUser(u), nil
}

func (s *Store) DeleteUser(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[username]; !exists {
		return ErrUserNotFound
	}
	delete(s.users, username)
	return s.saveLocked()
}

func (s *Store) GetByUsername(username string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	u, ok := s.users[username]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneUser(u), nil
}

func (s *Store) GetByID(id []byte) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, u := range s.users {
		if string(u.ID) == string(id) {
			return cloneUser(u), nil
		}
	}
	return nil, ErrUserNotFound
}

func (s *Store) VerifyPassword(username, password string) (*User, error) {
	s.mu.RLock()
	u, ok := s.users[username]
	s.mu.RUnlock()

	if !ok || u.PasswordHash == "" {
		return nil, ErrInvalidCredential
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredential
	}
	return cloneUser(u), nil
}

func (s *Store) UpdatePassword(username, newPassword string) error {
	if len(newPassword) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.users[username]
	if !ok {
		return ErrUserNotFound
	}
	u.PasswordHash = string(hash)
	return s.saveLocked()
}

func (s *Store) AddPasskey(username, name string, cred webauthn.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.users[username]
	if !ok {
		return ErrUserNotFound
	}
	if name == "" {
		name = fmt.Sprintf("Passkey %d", len(u.Passkeys)+1)
	}
	u.Passkeys = append(u.Passkeys, StoredCredential{
		Name:       name,
		CreatedAt:  time.Now().UTC(),
		Credential: cred,
	})
	return s.saveLocked()
}

func (s *Store) UpdatePasskey(username string, cred webauthn.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.users[username]
	if !ok {
		return ErrUserNotFound
	}
	for i := range u.Passkeys {
		if string(u.Passkeys[i].Credential.ID) == string(cred.ID) {
			u.Passkeys[i].Credential = cred
			return s.saveLocked()
		}
	}
	return errors.New("passkey not found")
}

func (s *Store) DeletePasskey(username string, credID []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.users[username]
	if !ok {
		return ErrUserNotFound
	}
	filtered := make([]StoredCredential, 0, len(u.Passkeys))
	found := false
	for _, pk := range u.Passkeys {
		if string(pk.Credential.ID) == string(credID) {
			found = true
			continue
		}
		filtered = append(filtered, pk)
	}
	if !found {
		return errors.New("passkey not found")
	}
	u.Passkeys = filtered
	return s.saveLocked()
}

func (s *Store) RevokePasskeys(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.users[username]
	if !ok {
		return ErrUserNotFound
	}
	u.Passkeys = nil
	return s.saveLocked()
}

func cloneUser(u *User) *User {
	cp := *u
	cp.ID = append([]byte(nil), u.ID...)
	cp.Passkeys = append([]StoredCredential(nil), u.Passkeys...)
	return &cp
}
