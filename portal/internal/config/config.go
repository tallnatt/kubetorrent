package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
)

type UserRoute struct {
	Username        string `json:"username"`
	Port            int    `json:"port"`
	BackendURL      string `json:"backend_url,omitempty"`
	InitialPassword string `json:"initial_password,omitempty"`
}

type Config struct {
	ListenAddr    string
	DataDir       string
	SessionSecret []byte
	RPID          string
	RPOrigins     []string
	RPDisplayName string
	BackendHost   string
	CookieSecure  bool
	Users         []UserRoute
}

func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:    getEnv("LISTEN_ADDR", ":8080"),
		DataDir:       getEnv("DATA_DIR", "/data/portal"),
		RPID:          getEnv("RP_ID", "localhost"),
		RPDisplayName: getEnv("RP_DISPLAY_NAME", "KubeTorrent"),
		BackendHost:   getEnv("BACKEND_HOST", "http://kubetorrent-clients"),
	}

	originsEnv := getEnv("RP_ORIGINS", "")
	if originsEnv != "" {
		for _, o := range strings.Split(originsEnv, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				cfg.RPOrigins = append(cfg.RPOrigins, o)
			}
		}
	} else {
		if cfg.RPID == "localhost" {
			cfg.RPOrigins = []string{"http://localhost:8080", "https://localhost:8080"}
		} else {
			cfg.RPOrigins = []string{"https://" + cfg.RPID}
		}
	}

	cookieSecureEnv := os.Getenv("COOKIE_SECURE")
	if cookieSecureEnv != "" {
		cfg.CookieSecure = strings.EqualFold(cookieSecureEnv, "true") || cookieSecureEnv == "1"
	} else {
		cfg.CookieSecure = cfg.RPID != "localhost"
	}

	secretEnv := os.Getenv("SESSION_SECRET")
	if secretEnv == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("failed to generate session secret: %w", err)
		}
		cfg.SessionSecret = b
		log.Println("WARNING: SESSION_SECRET not set; generated ephemeral secret (sessions will reset on restart)")
	} else {
		cfg.SessionSecret = []byte(secretEnv)
	}

	usersPath := getEnv("USERS_CONFIG_PATH", "/etc/kubetorrent/users.json")
	if usersJSON := os.Getenv("USERS_JSON"); usersJSON != "" {
		if err := json.Unmarshal([]byte(usersJSON), &cfg.Users); err != nil {
			return nil, fmt.Errorf("invalid USERS_JSON: %w", err)
		}
	} else if data, err := os.ReadFile(usersPath); err == nil {
		if err := json.Unmarshal(data, &cfg.Users); err != nil {
			return nil, fmt.Errorf("invalid %s: %w", usersPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read %s: %w", usersPath, err)
	}

	for i := range cfg.Users {
		if cfg.Users[i].BackendURL == "" && cfg.Users[i].Port > 0 {
			host := strings.TrimRight(cfg.BackendHost, "/")
			if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
				host = "http://" + host
			}
			cfg.Users[i].BackendURL = fmt.Sprintf("%s:%d", host, cfg.Users[i].Port)
		}
	}

	return cfg, nil
}

func (c *Config) SessionKeyHex() string {
	return hex.EncodeToString(c.SessionSecret)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
