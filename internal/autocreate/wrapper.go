// Package autocreate exports the public API for automatic account creation.
package autocreate

import (
	"github.com/deivid22srk/maritacaproxy/internal/account"
	"github.com/deivid22srk/maritacaproxy/internal/auth"
)

// AutoCreateConfig is the configuration for automatic account creation.
type AutoCreateConfig struct {
	Headless          bool
	ChromePath        string
	UserDataDir       string
	Password          string
	VerifyInterval    int
	VerifyMaxAttempts int
	TempMailProvider  string
	TempMailDomain    string
	Auth0Config       auth.Config
}

// NewCreator creates a new autocreate Creator with the given config and
// account manager.
func NewCreator(cfg AutoCreateConfig, mgr *account.Manager) (*Creator, error) {
	c := Config{
		Headless:          cfg.Headless,
		ChromePath:        cfg.ChromePath,
		UserDataDir:       cfg.UserDataDir,
		Password:          cfg.Password,
		VerifyInterval:    cfg.VerifyInterval,
		VerifyMaxAttempts: cfg.VerifyMaxAttempts,
		TempMailProvider:  cfg.TempMailProvider,
		TempMailDomain:    cfg.TempMailDomain,
		Auth0Config:       cfg.Auth0Config,
	}
	return New(c, mgr)
}
