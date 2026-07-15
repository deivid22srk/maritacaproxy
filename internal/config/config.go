// Package config provides configuration loading from environment variables.
package config

import (
        "fmt"
        "os"
        "path/filepath"
        "strconv"
        "strings"
)

// Config holds all runtime configuration for the proxy.
type Config struct {
        Server    ServerConfig
        Auth      AuthConfig
        Maritaca  MaritacaConfig
        TempMail  TempMailConfig
        AutoAcc   AutoAccountConfig
        Cache     CacheConfig
        Timeouts  TimeoutConfig
}

type ServerConfig struct {
        Port      int
        Host      string
        APIKey    string
        ProxyAuth bool
}

type AuthConfig struct {
        Auth0Domain   string
        Auth0ClientID string
        Audience      string
        Scope         string
        RedirectURI   string
        Connection    string
}

type MaritacaConfig struct {
        BaseURL    string
        ChatAPIURL string
}

type TempMailConfig struct {
        Provider string // "1secmail" or "tempmail"
        Domain   string // optional override domain
}

type AutoAccountConfig struct {
        Enabled           bool
        Headless          bool
        ChromePath        string
        UserDataDir       string
        Password          string
        MaxAccounts       int
        VerifyInterval    int // seconds between verification email checks
        VerifyMaxAttempts int
}

type CacheConfig struct {
        DefaultTTL int
}

type TimeoutConfig struct {
        HTTP       int
        Chat       int
        StreamIdle int
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (*Config, error) {
        cfg := &Config{
                Server: ServerConfig{
                        Port:      getEnvInt("PORT", 3000),
                        Host:      getEnv("HOST", "0.0.0.0"),
                        APIKey:    getEnv("API_KEY", ""),
                        ProxyAuth: getEnvBool("PROXY_AUTH", false),
                },
                Auth: AuthConfig{
                        Auth0Domain:   getEnv("AUTH0_DOMAIN", "auth.maritaca.ai"),
                        Auth0ClientID: getEnv("AUTH0_CLIENT_ID", "qBJrntH9D92AA5n0PR0ph1h54hSqP3C6"),
                        Audience:      getEnv("AUTH0_AUDIENCE", "https://chat.maritaca.ai/api"),
                        Scope:         getEnv("AUTH0_SCOPE", "openid profile email offline_access chat:messages"),
                        RedirectURI:   getEnv("AUTH0_REDIRECT_URI", "https://chat.maritaca.ai/auth"),
                        Connection:    getEnv("AUTH0_CONNECTION", "Username-Password-Authentication"),
                },
                Maritaca: MaritacaConfig{
                        BaseURL:    getEnv("MARITACA_BASE_URL", "https://chat.maritaca.ai"),
                        ChatAPIURL: getEnv("MARITACA_CHAT_API", "https://chat.maritaca.ai/api"),
                },
                TempMail: TempMailConfig{
                        Provider: getEnv("TEMPMAIL_PROVIDER", "mailtm"),
                        Domain:   getEnv("TEMPMAIL_DOMAIN", ""),
                },
                AutoAcc: AutoAccountConfig{
                        Enabled:           getEnvBool("AUTO_ACCOUNT_ENABLED", false),
                        Headless:          getEnvBool("AUTO_ACCOUNT_HEADLESS", true),
                        ChromePath:        getEnv("CHROME_PATH", ""),
                        UserDataDir:       getEnv("USER_DATA_DIR", "./maritaca_profiles"),
                        Password:          getEnv("AUTO_ACCOUNT_PASSWORD", "MaritacaProxy@2024"),
                        MaxAccounts:       getEnvInt("AUTO_ACCOUNT_MAX", 5),
                        VerifyInterval:    getEnvInt("AUTO_VERIFY_INTERVAL", 5),
                        VerifyMaxAttempts: getEnvInt("AUTO_VERIFY_MAX_ATTEMPTS", 60),
                },
                Cache: CacheConfig{
                        DefaultTTL: getEnvInt("CACHE_TTL", 3600),
                },
                Timeouts: TimeoutConfig{
                        HTTP:       getEnvInt("HTTP_TIMEOUT", 45000),
                        Chat:       getEnvInt("CHAT_TIMEOUT", 120000),
                        StreamIdle: getEnvInt("STREAM_IDLE_TIMEOUT", 180000),
                },
        }

        if cfg.AutoAcc.ChromePath == "" {
                // Auto-detect chrome across common locations
                home, _ := os.UserHomeDir()
                candidates := []string{
                        "/home/z/.cache/ms-playwright/chromium-1228/chrome-linux64/chrome",
                        "/home/z/.cache/ms-playwright/chromium-1200/chrome-linux64/chrome",
                        "/root/.cache/ms-playwright/chromium-1228/chrome-linux64/chrome",
                        "/root/.cache/ms-playwright/chromium-1200/chrome-linux64/chrome",
                        "/usr/bin/google-chrome",
                        "/usr/bin/chromium",
                        "/usr/bin/chromium-browser",
                }
                if home != "" {
                        candidates = append([]string{
                                home + "/.cache/ms-playwright/chromium-1228/chrome-linux64/chrome",
                                home + "/.cache/ms-playwright/chromium-1200/chrome-linux64/chrome",
                        }, candidates...)
                }
                // Also try a glob for any playwright chromium version
                globMatches, _ := filepath.Glob(home + "/.cache/ms-playwright/chromium-*/chrome-linux64/chrome")
                candidates = append(candidates, globMatches...)
                globMatches2, _ := filepath.Glob(home + "/.cache/ms-playwright/chromium-*/chrome-linux/chrome")
                candidates = append(candidates, globMatches2...)
                for _, p := range candidates {
                        if _, err := os.Stat(p); err == nil {
                                cfg.AutoAcc.ChromePath = p
                                break
                        }
                }
        }

        return cfg, nil
}

func (c *Config) String() string {
        return fmt.Sprintf("Config{Server:%d, Auth0:%s, Maritaca:%s, AutoAcc:%v/%s}",
                c.Server.Port, c.Auth.Auth0Domain, c.Maritaca.BaseURL,
                c.AutoAcc.Enabled, c.AutoAcc.ChromePath)
}

func getEnv(key, def string) string {
        if v := os.Getenv(key); v != "" {
                return v
        }
        return def
}

func getEnvInt(key string, def int) int {
        if v := os.Getenv(key); v != "" {
                if i, err := strconv.Atoi(v); err == nil {
                        return i
                }
        }
        return def
}

func getEnvBool(key string, def bool) bool {
        if v := os.Getenv(key); v != "" {
                if b, err := strconv.ParseBool(strings.ToLower(v)); err == nil {
                        return b
                }
        }
        return def
}
