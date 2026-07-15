// Package main is the entry point for the maritacaproxy server.
package main

import (
        "context"
        "encoding/json"
        "flag"
        "fmt"
        "net/http"
        "os"
        "os/signal"
        "strconv"
        "syscall"
        "time"

        "github.com/deivid22srk/maritacaproxy/internal/account"
        "github.com/deivid22srk/maritacaproxy/internal/api"
        "github.com/deivid22srk/maritacaproxy/internal/auth"
        "github.com/deivid22srk/maritacaproxy/internal/autocreate"
        "github.com/deivid22srk/maritacaproxy/internal/config"
        "github.com/deivid22srk/maritacaproxy/internal/logger"
        "github.com/deivid22srk/maritacaproxy/internal/tools"
)

func main() {
        var (
                createAccount bool
                createCount   int
        )
        flag.BoolVar(&createAccount, "create-account", false, "Create N new Maritaca accounts and exit")
        flag.IntVar(&createCount, "count", 1, "Number of accounts to create (with -create-account)")
        flag.Parse()

        cfg, err := config.Load()
        if err != nil {
                fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
                os.Exit(1)
        }
        logger.Info("[main] %s", cfg.String())

        // Configure the tool-call tag name (must be done before any parser is constructed)
        tools.SetTagName(cfg.Tools.TagName)
        logger.Info("[main] Tool-call tag: <%s>", cfg.Tools.TagName)

        mgr, err := account.NewManager("./data/maritacaproxy.db.json")
        if err != nil {
                logger.Error("[main] Failed to load account manager: %v", err)
                os.Exit(1)
        }

        authCfg := auth.Config{
                Domain:      cfg.Auth.Auth0Domain,
                ClientID:    cfg.Auth.Auth0ClientID,
                Audience:    cfg.Auth.Audience,
                Scope:       cfg.Auth.Scope,
                RedirectURI: cfg.Auth.RedirectURI,
                Connection:  cfg.Auth.Connection,
        }

        if createAccount {
                if !cfg.AutoAcc.Enabled {
                        logger.Warn("[main] AUTO_ACCOUNT_ENABLED=false; creating anyway due to --create-account flag")
                }
                creator, err := autocreate.NewCreator(autocreate.AutoCreateConfig{
                        Headless:          cfg.AutoAcc.Headless,
                        ChromePath:        cfg.AutoAcc.ChromePath,
                        UserDataDir:       cfg.AutoAcc.UserDataDir,
                        Password:          cfg.AutoAcc.Password,
                        VerifyInterval:    cfg.AutoAcc.VerifyInterval,
                        VerifyMaxAttempts: cfg.AutoAcc.VerifyMaxAttempts,
                        TempMailProvider:  cfg.TempMail.Provider,
                        TempMailDomain:    cfg.TempMail.Domain,
                        Auth0Config:       authCfg,
                }, mgr)
                if err != nil {
                        logger.Error("[main] Failed to init autocreate: %v", err)
                        os.Exit(1)
                }
                ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
                defer cancel()
                created, err := creator.CreateMany(ctx, createCount)
                logger.Info("[main] Created %d accounts (err=%v)", len(created), err)
                return
        }

        srv := api.NewServer(mgr, cfg.Maritaca.BaseURL, cfg.Server.APIKey, authCfg)

        mux := http.NewServeMux()
        srv.RegisterRoutes(mux)

        // Override the /v1/accounts/create endpoint with the autocreate-enabled version
        if cfg.AutoAcc.Enabled {
                creator, err := autocreate.NewCreator(autocreate.AutoCreateConfig{
                        Headless:          cfg.AutoAcc.Headless,
                        ChromePath:        cfg.AutoAcc.ChromePath,
                        UserDataDir:       cfg.AutoAcc.UserDataDir,
                        Password:          cfg.AutoAcc.Password,
                        VerifyInterval:    cfg.AutoAcc.VerifyInterval,
                        VerifyMaxAttempts: cfg.AutoAcc.VerifyMaxAttempts,
                        TempMailProvider:  cfg.TempMail.Provider,
                        TempMailDomain:    cfg.TempMail.Domain,
                        Auth0Config:       authCfg,
                }, mgr)
                if err != nil {
                        logger.Error("[main] Failed to init autocreate: %v", err)
                } else {
                        mux.HandleFunc("/v1/accounts/create", func(w http.ResponseWriter, r *http.Request) {
                                if r.Method != http.MethodPost {
                                        w.WriteHeader(http.StatusMethodNotAllowed)
                                        return
                                }
                                var body struct {
                                        Count int `json:"count"`
                                }
                                if r.Body != nil {
                                        _ = json.NewDecoder(r.Body).Decode(&body)
                                }
                                if body.Count <= 0 {
                                        body.Count = 1
                                }
                                go func() {
                                        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
                                        defer cancel()
                                        _, err := creator.CreateMany(ctx, body.Count)
                                        if err != nil {
                                                logger.Error("[accounts] Auto-create failed: %v", err)
                                        }
                                }()
                                w.Header().Set("Content-Type", "application/json")
                                w.WriteHeader(http.StatusAccepted)
                                json.NewEncoder(w).Encode(map[string]interface{}{
                                        "message": "account creation started in background",
                                        "count":   body.Count,
                                })
                        })
                        logger.Info("[main] Auto account creation enabled at /v1/accounts/create")
                }
        }

        // Wrap with auth middleware if API key is set
        handler := http.Handler(mux)
        if cfg.Server.APIKey != "" {
                handler = wrapAuth(mux, cfg.Server.APIKey)
        }

        addr := cfg.Server.Host + ":" + strconv.Itoa(cfg.Server.Port)
        httpSrv := &http.Server{
                Addr:              addr,
                Handler:           handler,
                ReadHeaderTimeout: 30 * time.Second,
        }

        go func() {
                logger.Info("[main] MaritacaProxy listening on http://%s", addr)
                if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                        logger.Error("[main] Server error: %v", err)
                        os.Exit(1)
                }
        }()

        // Graceful shutdown
        stop := make(chan os.Signal, 1)
        signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
        <-stop
        logger.Info("[main] Shutting down...")
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        _ = httpSrv.Shutdown(ctx)
        logger.Info("[main] Server stopped")
}

func wrapAuth(h http.Handler, apiKey string) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                // Skip auth for health and root
                if r.URL.Path == "/health" || r.URL.Path == "/" {
                        h.ServeHTTP(w, r)
                        return
                }
                if r.URL.Path == "/favicon.ico" {
                        h.ServeHTTP(w, r)
                        return
                }
                ah := r.Header.Get("Authorization")
                if ah == "" || ah[:7] != "Bearer " || ah[7:] != apiKey {
                        w.Header().Set("Content-Type", "application/json")
                        w.WriteHeader(http.StatusUnauthorized)
                        json.NewEncoder(w).Encode(map[string]interface{}{
                                "error": map[string]string{"message": "Invalid or missing API key"},
                        })
                        return
                }
                h.ServeHTTP(w, r)
        })
}
