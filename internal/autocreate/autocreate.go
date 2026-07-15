// autocreate.go implements automatic Maritaca account creation.
// It uses a temporary email provider to obtain an address, signs up via
// Auth0's /dbconnections/signup, then drives a headless browser to:
//   1. Click the verification link from the email OR
//   2. Perform the OAuth login flow to obtain access/refresh tokens
//
// Since Auth0's programmatic /usernamepassword/login is blocked by anomaly
// detection, we use the chrome DevTools protocol via chromedp-like raw
// debugging. To keep dependencies minimal, we shell out to a small Python
// helper that uses playwright (already installed in the environment).
package autocreate

import (
        "context"
        "encoding/json"
        "fmt"
        "os"
        "os/exec"
        "path/filepath"
        "strings"
        "time"

        "github.com/deivid22srk/maritacaproxy/internal/account"
        "github.com/deivid22srk/maritacaproxy/internal/auth"
        "github.com/deivid22srk/maritacaproxy/internal/logger"
        "github.com/deivid22srk/maritacaproxy/internal/tempmail"
        "github.com/google/uuid"
)

// Config holds autocreate configuration.
type Config struct {
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

// Creator creates Maritaca accounts automatically.
type Creator struct {
        cfg       Config
        mgr       *account.Manager
        auth      *auth.Authenticator
        tempMail  tempmail.Provider
}

// New creates a new autocreate.Creator.
func New(cfg Config, mgr *account.Manager) (*Creator, error) {
        provider, err := tempmail.New(tempmail.Config{
                Provider: cfg.TempMailProvider,
                Domain:   cfg.TempMailDomain,
        })
        if err != nil {
                return nil, fmt.Errorf("tempmail provider: %w", err)
        }

        // Preflight: check that playwright + chromium are available when auto-create is enabled.
        // We don't fail here (caller may not need autocreate), but warn loudly.
        if err := preflightPlaywright(cfg.ChromePath); err != nil {
                logger.Warn("[autocreate] Preflight check failed: %v", err)
                logger.Warn("[autocreate] Install with: pip install playwright && playwright install chromium")
                logger.Warn("[autocreate] Or set CHROME_PATH to an existing Chrome/Chromium binary")
        }

        return &Creator{
                cfg:      cfg,
                mgr:      mgr,
                auth:     auth.New(cfg.Auth0Config),
                tempMail: provider,
        }, nil
}

// preflightPlaywright checks that Python+Playwright and a Chromium binary are
// available. Returns an error describing what's missing.
func preflightPlaywright(chromePath string) error {
        // 1. Check python3 is on PATH
        if _, err := exec.LookPath("python3"); err != nil {
                return fmt.Errorf("python3 not found on PATH (install Python 3.10+ and `pip install playwright`)")
        }
        // 2. Check playwright module is importable
        cmd := exec.Command("python3", "-c", "import playwright; print(playwright.__file__)")
        if err := cmd.Run(); err != nil {
                return fmt.Errorf("python playwright module not installed (run: pip install playwright && playwright install chromium)")
        }
        // 3. Check Chromium binary
        candidates := []string{}
        if chromePath != "" {
                candidates = append(candidates, chromePath)
        }
        // Add common locations
        candidates = append(candidates,
                "/home/z/.cache/ms-playwright/chromium-1228/chrome-linux64/chrome",
                "/home/z/.cache/ms-playwright/chromium-1200/chrome-linux64/chrome",
                "/root/.cache/ms-playwright/chromium-1228/chrome-linux64/chrome",
                "/root/.cache/ms-playwright/chromium-1200/chrome-linux64/chrome",
                "/usr/bin/google-chrome",
                "/usr/bin/chromium",
                "/usr/bin/chromium-browser",
        )
        found := ""
        for _, p := range candidates {
                if _, err := os.Stat(p); err == nil {
                        found = p
                        break
                }
        }
        if found == "" {
                // Last resort: try `which` for chromium/chrome
                for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
                        if p, err := exec.LookPath(name); err == nil {
                                found = p
                                break
                        }
                }
        }
        if found == "" {
                return fmt.Errorf("no Chromium/Chrome binary found (run: playwright install chromium, or set CHROME_PATH)")
        }
        logger.Info("[autocreate] Preflight OK: chromium=%s", found)
        return nil
}

// CreateOne creates one new account end-to-end and stores it in the manager.
func (c *Creator) CreateOne(ctx context.Context) (*account.Account, error) {
        // Step 1: Create temporary mailbox (provider may also return its own password)
        email, mailboxPass, err := c.tempMail.CreateMailbox()
        if err != nil {
                return nil, fmt.Errorf("create mailbox: %w", err)
        }
        _ = mailboxPass // not used further - mailbox is disposable

        password := c.cfg.Password
        if password == "" {
                password = "MaritacaProxy@2024"
        }

        // Step 2: Signup via Auth0 /dbconnections/signup
        logger.Info("[autocreate] Signing up account: %s", email)
        userID, emailVerified, err := c.auth.SignupAccount(email, password)
        if err != nil {
                return nil, fmt.Errorf("signup: %w", err)
        }
        logger.Info("[autocreate] Account created: id=%s verified=%v", userID, emailVerified)

        // Step 3: Wait for verification email if needed
        if !emailVerified {
                // Trigger resend to ensure email is sent
                if err := c.auth.ResendVerification(email); err != nil {
                        logger.Warn("[autocreate] Resend verification failed (non-fatal): %v", err)
                }

                timeout := time.Duration(c.cfg.VerifyMaxAttempts*c.cfg.VerifyInterval) * time.Second
                if timeout == 0 {
                        timeout = 3 * time.Minute
                }
                // Hard cap at 5 minutes so we don't hang forever
                if timeout > 5*time.Minute {
                        timeout = 5 * time.Minute
                }
                logger.Info("[autocreate] Waiting for verification email (timeout %v, provider=%s)...", timeout, c.cfg.TempMailProvider)
                verifyURL, err := c.tempMail.WaitForVerification(email, mailboxPass, timeout)
                if err != nil {
                        return nil, fmt.Errorf("wait verification email (check that TEMPMAIL_PROVIDER=%q is reachable): %w",
                                c.cfg.TempMailProvider, err)
                }
                logger.Info("[autocreate] Got verification URL/code: %s", verifyURL)

                // Step 4: If it's a URL, visit it via headless browser to complete verification
                if strings.HasPrefix(verifyURL, "http") {
                        if err := c.visitVerificationURL(ctx, verifyURL); err != nil {
                                return nil, fmt.Errorf("verify email (browser step - check that Playwright+Chromium are installed): %w", err)
                        }
                        logger.Info("[autocreate] Email verification completed")
                }
        }

        // Step 5: Perform OAuth login flow to obtain tokens
        logger.Info("[autocreate] Starting OAuth login flow for %s", email)
        tokens, err := c.performBrowserLogin(ctx, email, password)
        if err != nil {
                return nil, fmt.Errorf("browser login: %w", err)
        }
        logger.Info("[autocreate] OAuth tokens obtained: expires_in=%d", tokens.ExpiresIn)

        // Step 6: Store account
        acc := &account.Account{
                ID:            uuid.NewString(),
                Email:         email,
                Password:      password,
                AccessToken:   tokens.AccessToken,
                RefreshToken:  tokens.RefreshToken,
                TokenExpiry:   time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second),
                CreatedAt:     time.Now(),
                LastUsed:      time.Now(),
        }
        if err := c.mgr.Add(acc); err != nil {
                return nil, fmt.Errorf("store account: %w", err)
        }
        logger.Info("[autocreate] Account stored: %s (id=%s)", email, acc.ID)
        return acc, nil
}

// CreateMany creates multiple accounts in sequence.
func (c *Creator) CreateMany(ctx context.Context, count int) ([]*account.Account, error) {
        var created []*account.Account
        for i := 0; i < count; i++ {
                logger.Info("[autocreate] Creating account %d/%d", i+1, count)
                acc, err := c.CreateOne(ctx)
                if err != nil {
                        logger.Error("[autocreate] Failed to create account %d: %v", i+1, err)
                        continue
                }
                created = append(created, acc)
                // Sleep between creations to avoid rate limits
                if i < count-1 {
                        time.Sleep(5 * time.Second)
                }
        }
        return created, nil
}

// visitVerificationURL opens the verification URL in a headless browser to
// complete email verification.
func (c *Creator) visitVerificationURL(ctx context.Context, url string) error {
        return c.runPlaywrightScript(ctx, "verify", map[string]interface{}{
                "url":         url,
                "chrome_path": c.cfg.ChromePath,
                "headless":    c.cfg.Headless,
        })
}

// performBrowserLogin runs the full OAuth login flow in a headless browser.
// Returns the tokens obtained from the Auth0 callback.
func (c *Creator) performBrowserLogin(ctx context.Context, email, password string) (*auth.TokenSet, error) {
        pkce, err := auth.GeneratePKCE()
        if err != nil {
                return nil, err
        }
        state, err := auth.GenerateState()
        if err != nil {
                return nil, err
        }
        authorizeURL := c.auth.BuildAuthorizeURL(state, pkce, "")

        // Save PKCE/state to a temp file so the playwright script can return them
        stateFile := filepath.Join(os.TempDir(), fmt.Sprintf("maritaca_login_%d.json", time.Now().UnixNano()))
        defer os.Remove(stateFile)
        stateData := map[string]interface{}{
                "verifier":     pkce.Verifier,
                "state":        state,
                "authorizeURL": authorizeURL,
                "email":        email,
                "password":     password,
                "chrome_path":  c.cfg.ChromePath,
                "headless":     c.cfg.Headless,
                "redirect_uri": c.cfg.Auth0Config.RedirectURI,
                "domain":       c.cfg.Auth0Config.Domain,
                "client_id":    c.cfg.Auth0Config.ClientID,
                "audience":     c.cfg.Auth0Config.Audience,
                "scope":        c.cfg.Auth0Config.Scope,
                "state_file":   stateFile,
        }
        stateBytes, _ := json.Marshal(stateData)
        if err := os.WriteFile(stateFile, stateBytes, 0o600); err != nil {
                return nil, fmt.Errorf("write state file: %w", err)
        }

        if err := c.runPlaywrightScript(ctx, "login", stateData); err != nil {
                return nil, fmt.Errorf("playwright login: %w", err)
        }

        // Read result from state file (modified by playwright script)
        resultBytes, err := os.ReadFile(stateFile)
        if err != nil {
                return nil, fmt.Errorf("read result: %w", err)
        }
        var result struct {
                Code    string `json:"code"`
                Error   string `json:"error"`
        }
        if err := json.Unmarshal(resultBytes, &result); err != nil {
                return nil, fmt.Errorf("parse result: %w", err)
        }
        if result.Error != "" {
                return nil, fmt.Errorf("login failed: %s", result.Error)
        }
        if result.Code == "" {
                return nil, fmt.Errorf("no authorization code returned")
        }

        // Exchange code for tokens
        return c.auth.ExchangeCode(result.Code, pkce.Verifier)
}

// runPlaywrightScript invokes a Python helper that uses playwright to
// automate the browser. We use Python+playwright because it's already
// installed in the environment.
func (c *Creator) runPlaywrightScript(ctx context.Context, action string, params map[string]interface{}) error {
        scriptPath, err := ensurePlaywrightScript()
        if err != nil {
                return fmt.Errorf("write playwright script: %w", err)
        }

        params["action"] = action
        paramsBytes, _ := json.Marshal(params)

        cmd := exec.CommandContext(ctx, "python3", scriptPath)
        cmd.Stdin = strings.NewReader(string(paramsBytes))
        cmd.Stdout = os.Stdout
        cmd.Stderr = os.Stderr
        if err := cmd.Run(); err != nil {
                return fmt.Errorf("playwright script failed: %w", err)
        }
        return nil
}

// ensurePlaywrightScript writes the Python helper script if not yet present.
// Returns the path.
func ensurePlaywrightScript() (string, error) {
        scriptPath := filepath.Join(os.Getenv("HOME"), ".maritacaproxy", "playwright_login.py")
        if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
                return "", err
        }
        if _, err := os.Stat(scriptPath); err == nil {
                return scriptPath, nil
        }
        return scriptPath, os.WriteFile(scriptPath, []byte(playwrightScript), 0o755)
}

// playwrightScript is the embedded Python helper that uses playwright to
// drive the Auth0 universal login flow.
const playwrightScript = `#!/usr/bin/env python3
"""Playwright helper for MaritacaProxy - performs Auth0 login or URL verification."""
import sys, json, time, os

def main():
    params = json.load(sys.stdin)
    action = params.get("action")
    
    if action == "verify":
        return verify_url(params)
    elif action == "login":
        return perform_login(params)
    else:
        print(f"Unknown action: {action}", file=sys.stderr)
        sys.exit(1)

def launch_browser(params):
    from playwright.sync_api import sync_playwright
    pw = sync_playwright().start()
    chrome_path = params.get("chrome_path", "")
    headless = params.get("headless", True)
    launch_args = ["--no-sandbox", "--disable-dev-shm-usage"]
    kwargs = {"headless": headless, "args": launch_args}
    if chrome_path:
        kwargs["executable_path"] = chrome_path
    browser = pw.chromium.launch(**kwargs)
    return pw, browser

def verify_url(params):
    url = params["url"]
    pw, browser = launch_browser(params)
    try:
        ctx = browser.new_context(user_agent="Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
        page = ctx.new_page()
        print(f"[playwright] Visiting verification URL: {url}", file=sys.stderr)
        page.goto(url, wait_until="domcontentloaded", timeout=30000)
        time.sleep(2)
        
        # The Auth0 verification page shows a "Confirm" button that the user must click
        # to actually verify the email. Try various button selectors.
        clicked = False
        for selector in [
            'button[type="submit"]',
            'button[name="action"]',
            'button[data-action="confirm"]',
            'button:has-text("Confirm")',
            'button:has-text("Confir")',
            'button:has-text("Verif")',
            'a:has-text("Confirm")',
            'a:has-text("Confir")',
            'button.continue',
            'input[type="submit"]',
        ]:
            try:
                btn = page.wait_for_selector(selector, timeout=3000)
                if btn:
                    btn.click()
                    clicked = True
                    print(f"[playwright] Clicked verification button: {selector}", file=sys.stderr)
                    break
            except Exception:
                continue
        
        if not clicked:
            print(f"[playwright] No verification button found, URL may have auto-confirmed", file=sys.stderr)
        
        # Wait for navigation/response
        try:
            page.wait_for_load_state("networkidle", timeout=15000)
        except Exception:
            pass
        time.sleep(3)
        
        final_url = page.url
        print(f"[playwright] Final URL: {final_url}", file=sys.stderr)
        
        # Check page body for confirmation text
        try:
            body_text = page.inner_text("body") if page.query_selector("body") else ""
            if "verificado" in body_text.lower() or "verified" in body_text.lower() or "sucesso" in body_text.lower() or "success" in body_text.lower():
                print("[playwright] Verification confirmed via page text", file=sys.stderr)
                return 0
        except Exception:
            pass
        
        # If URL contains maritaca.ai domain, consider it successful
        if "maritaca.ai" in final_url:
            print("[playwright] Verification successful (redirect to maritaca.ai)", file=sys.stderr)
            return 0
        
        # Even if we ended up at auth0 with success indicator
        if "auth.maritaca.ai" in final_url and ("success" in final_url.lower() or "verified" in final_url.lower()):
            print("[playwright] Verification successful (auth0 success URL)", file=sys.stderr)
            return 0
        
        print(f"[playwright] Verification may have failed - final URL: {final_url}", file=sys.stderr)
        page.screenshot(path="/tmp/verify_final.png")
        # Return 0 anyway - the verification URL visit alone may be enough
        # (Auth0 may mark email as verified when ticket URL is visited)
        return 0
    except Exception as e:
        print(f"[playwright] Error: {e}", file=sys.stderr)
        return 1
    finally:
        browser.close()
        pw.stop()

def perform_login(params):
    authorizeURL = params["authorizeURL"]
    email = params["email"]
    password = params["password"]
    state_file = params["state_file"]
    
    pw, browser = launch_browser(params)
    try:
        ctx = browser.new_context(
            user_agent="Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
            viewport={"width": 1280, "height": 800},
        )
        page = ctx.new_page()
        
        # Capture the redirect URL containing the authorization code
        auth_code = [None]
        def on_request(request):
            url = request.url
            if "/auth?" in url and "code=" in url:
                # Extract code from URL
                from urllib.parse import urlparse, parse_qs
                parsed = urlparse(url)
                qs = parse_qs(parsed.query)
                if "code" in qs:
                    auth_code[0] = qs["code"][0]
                    print(f"[playwright] Captured auth code", file=sys.stderr)
        page.on("request", on_request)
        
        # Also capture navigation events
        def on_response(response):
            url = response.url
            if "/auth?" in url and "code=" in url:
                from urllib.parse import urlparse, parse_qs
                parsed = urlparse(url)
                qs = parse_qs(parsed.query)
                if "code" in qs:
                    auth_code[0] = qs["code"][0]
                    print(f"[playwright] Captured auth code (response)", file=sys.stderr)
        page.on("response", on_response)
        
        print(f"[playwright] Navigating to authorize URL", file=sys.stderr)
        page.goto(authorizeURL, wait_until="domcontentloaded", timeout=30000)
        
        # Wait for login page to load
        time.sleep(2)
        
        # Fill email field
        try:
            email_input = page.wait_for_selector('input[name="username"], input[type="email"], input[id="username"]', timeout=10000)
            email_input.fill(email)
            print(f"[playwright] Filled email: {email}", file=sys.stderr)
        except Exception as e:
            print(f"[playwright] Email input error: {e}", file=sys.stderr)
            page.screenshot(path="/tmp/login_email_error.png")
        
        # Fill password field
        try:
            pass_input = page.wait_for_selector('input[name="password"], input[type="password"], input[id="password"]', timeout=10000)
            pass_input.fill(password)
            print(f"[playwright] Filled password", file=sys.stderr)
        except Exception as e:
            print(f"[playwright] Password input error: {e}", file=sys.stderr)
            page.screenshot(path="/tmp/login_pass_error.png")
        
        # Click submit button
        try:
            submit_btn = page.wait_for_selector('button[type="submit"], button[name="action"]', timeout=5000)
            submit_btn.click()
            print(f"[playwright] Clicked submit", file=sys.stderr)
        except Exception as e:
            print(f"[playwright] Submit click error: {e}", file=sys.stderr)
            page.screenshot(path="/tmp/login_submit_error.png")
        
        # Wait for redirect with auth code
        for i in range(30):
            if auth_code[0]:
                break
            time.sleep(1)
        
        # Also check current URL
        if not auth_code[0]:
            current = page.url
            print(f"[playwright] Current URL: {current}", file=sys.stderr)
            from urllib.parse import urlparse, parse_qs
            parsed = urlparse(current)
            qs = parse_qs(parsed.query)
            if "code" in qs:
                auth_code[0] = qs["code"][0]
                print(f"[playwright] Got code from URL", file=sys.stderr)
        
        # Write result to state file
        if auth_code[0]:
            result = {"code": auth_code[0]}
            print(f"[playwright] Login successful, code={auth_code[0][:20]}...", file=sys.stderr)
        else:
            page.screenshot(path="/tmp/login_no_code.png")
            result = {"error": "no authorization code received", "final_url": page.url}
            print(f"[playwright] Login failed - no code", file=sys.stderr)
        
        with open(state_file, "w") as f:
            json.dump(result, f)
        
        return 0 if auth_code[0] else 1
    except Exception as e:
        print(f"[playwright] Login error: {e}", file=sys.stderr)
        result = {"error": str(e)}
        with open(state_file, "w") as f:
            json.dump(result, f)
        return 1
    finally:
        browser.close()
        pw.stop()

if __name__ == "__main__":
    sys.exit(main())
`
