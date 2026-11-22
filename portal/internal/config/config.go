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
	Port            int    `json:"port,omitempty"`
	BackendURL      string `json:"backend_url,omitempty"`
	InitialPassword string `json:"initial_password,omitempty"`
	IsAdmin         bool   `json:"is_admin,omitempty"`
}

type Config struct {
	ListenAddr           string
	DataDir              string
	SessionSecret        []byte
	RPID                 string
	RPOrigins            []string
	RPDisplayName        string
	Namespace            string
	GatewayHost          string
	HostDownloadsPath    string
	ConfigsPVCName       string
	PodGatewayImage      string
	QBittorrentImage     string
	NotRoutedCIDRs       string
	VXLANID              string
	VXLANNetwork         string
	ClientServiceAccount string
	AdminUsername        string
	AdminInitialPassword string
	CookieSecure         bool
	Users                []UserRoute
}

func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:           getEnv("LISTEN_ADDR", ":8080"),
		DataDir:              getEnv("DATA_DIR", "/data/portal"),
		RPID:                 getEnv("RP_ID", "localhost"),
		RPDisplayName:        getEnv("RP_DISPLAY_NAME", "KubeTorrent"),
		Namespace:            getEnv("K8S_NAMESPACE", "kubetorrent"),
		GatewayHost:          getEnv("GATEWAY_HOST", "kubetorrent-vpn-gateway.kubetorrent.svc.cluster.local"),
		HostDownloadsPath:    getEnv("HOST_DOWNLOADS_PATH", "/srv/kubetorrent/downloads"),
		ConfigsPVCName:       getEnv("CONFIGS_PVC_NAME", "kubetorrent-configs"),
		PodGatewayImage:      getEnv("POD_GATEWAY_IMAGE", "ghcr.io/angelnu/pod-gateway:v1.13.0"),
		QBittorrentImage:     getEnv("QBITTORRENT_IMAGE", "lscr.io/linuxserver/qbittorrent:latest"),
		NotRoutedCIDRs:       getEnv("NOT_ROUTED_TO_GATEWAY_CIDRS", "10.0.0.0/8 172.16.0.0/12 192.168.0.0/16"),
		VXLANID:              getEnv("VXLAN_ID", "42"),
		VXLANNetwork:         getEnv("VXLAN_IP_NETWORK", "172.29.0"),
		ClientServiceAccount: os.Getenv("CLIENT_SERVICE_ACCOUNT"),
		AdminUsername:        getEnv("ADMIN_USERNAME", "admin"),
		AdminInitialPassword: os.Getenv("ADMIN_INITIAL_PASSWORD"),
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
		if cfg.Users[i].BackendURL == "" && !cfg.Users[i].IsAdmin {
			port := cfg.Users[i].Port
			if port == 0 {
				port = 8080
			}
			cfg.Users[i].BackendURL = cfg.DefaultUserBackendURL(cfg.Users[i].Username, port)
		}
	}

	if cfg.AdminInitialPassword != "" {
		foundAdmin := false
		for _, u := range cfg.Users {
			if u.Username == cfg.AdminUsername {
				foundAdmin = true
				break
			}
		}
		if !foundAdmin {
			cfg.Users = append(cfg.Users, UserRoute{
				Username:        cfg.AdminUsername,
				InitialPassword: cfg.AdminInitialPassword,
				IsAdmin:         true,
			})
		}
	}

	return cfg, nil
}

func (c *Config) DefaultUserBackendURL(username string, port int) string {
	if port == 0 {
		port = 8080
	}
	return fmt.Sprintf("http://kubetorrent-client-%s.%s.svc.cluster.local:%d", username, c.Namespace, port)
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
