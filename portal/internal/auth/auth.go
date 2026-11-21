package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	SessionCookieName = "kubetorrent_session"
	ChallengeCookie   = "kubetorrent_challenge"
	SessionTTL        = 24 * time.Hour
	ChallengeTTL      = 5 * time.Minute
)

type SessionPayload struct {
	Username string `json:"u"`
	Exp      int64  `json:"exp"`
}

type ChallengeEntry struct {
	Data     *webauthn.SessionData
	Username string
	Expires  time.Time
}

type Manager struct {
	secret       []byte
	cookieSecure bool

	mu         sync.Mutex
	challenges map[string]ChallengeEntry
}

func NewManager(secret []byte, cookieSecure bool) *Manager {
	return &Manager{
		secret:       secret,
		cookieSecure: cookieSecure,
		challenges:   make(map[string]ChallengeEntry),
	}
}

func (m *Manager) SetSessionCookie(w http.ResponseWriter, username string) error {
	payload := SessionPayload{
		Username: username,
		Exp:      time.Now().Add(SessionTTL).Unix(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	sig := m.sign(encoded)
	token := encoded + "." + sig

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL.Seconds()),
	})
	return nil
}

func (m *Manager) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   m.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (m *Manager) GetSessionUser(r *http.Request) (string, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return "", errors.New("missing session cookie")
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return "", errors.New("malformed session cookie")
	}
	if !hmac.Equal([]byte(m.sign(parts[0])), []byte(parts[1])) {
		return "", errors.New("invalid session signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", errors.New("invalid session encoding")
	}
	var payload SessionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", errors.New("invalid session payload")
	}
	if time.Now().Unix() > payload.Exp || payload.Username == "" {
		return "", errors.New("session expired")
	}
	return payload.Username, nil
}

func (m *Manager) SaveChallenge(w http.ResponseWriter, username string, data *webauthn.SessionData) error {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	id := hex.EncodeToString(b)

	m.mu.Lock()
	now := time.Now()
	for k, v := range m.challenges {
		if now.After(v.Expires) {
			delete(m.challenges, k)
		}
	}
	m.challenges[id] = ChallengeEntry{
		Data:     data,
		Username: username,
		Expires:  now.Add(ChallengeTTL),
	}
	m.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     ChallengeCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ChallengeTTL.Seconds()),
	})
	return nil
}

func (m *Manager) PopChallenge(w http.ResponseWriter, r *http.Request) (ChallengeEntry, error) {
	c, err := r.Cookie(ChallengeCookie)
	if err != nil {
		return ChallengeEntry{}, errors.New("missing challenge cookie")
	}
	http.SetCookie(w, &http.Cookie{
		Name:     ChallengeCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   m.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	m.mu.Lock()
	entry, ok := m.challenges[c.Value]
	if ok {
		delete(m.challenges, c.Value)
	}
	m.mu.Unlock()

	if !ok || time.Now().After(entry.Expires) {
		return ChallengeEntry{}, errors.New("challenge expired or not found")
	}
	return entry, nil
}

func (m *Manager) sign(data string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
