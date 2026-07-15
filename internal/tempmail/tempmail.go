// Package tempmail implements temporary email providers for automated
// account creation flows. It abstracts mailbox creation, polling, and OTP
// extraction across multiple providers.
package tempmail

import (
        "encoding/json"
        "fmt"
        "io"
        "math/rand"
        "net/http"
        "regexp"
        "strings"
        "time"

        "github.com/deivid22srk/maritacaproxy/internal/logger"
)

// Provider is the interface implemented by all temporary mail providers.
type Provider interface {
        // CreateMailbox provisions a new mailbox and returns the email address.
        CreateMailbox() (string, string, error) // returns email, password
        // WaitForVerification polls for an email containing a verification link/code.
        // Returns the extracted verification URL or code.
        WaitForVerification(email, password string, timeout time.Duration) (string, error)
        // Close releases any provider-specific resources.
        Close() error
}

// Config holds provider-specific configuration.
type Config struct {
        Provider string
        Domain   string
}

// New constructs a Provider based on the configured name.
func New(cfg Config) (Provider, error) {
        switch cfg.Provider {
        case "", "mailtm":
                return NewMailTM(), nil
        case "guerrillamail":
                return NewGuerrillaMail(), nil
        case "1secmail":
                return NewOneSecMail(cfg.Domain), nil
        default:
                return nil, fmt.Errorf("unknown tempmail provider: %s", cfg.Provider)
        }
}

// ─── Mail.tm ────────────────────────────────────────────────────────────────

// MailTM uses the mail.tm API. Free, reliable, and exposes JSON.
type MailTM struct {
        client *http.Client
        token  string // JWT token after login
}

// NewMailTM creates a Mail.tm provider.
func NewMailTM() *MailTM {
        return &MailTM{
                client: &http.Client{Timeout: 15 * time.Second},
        }
}

func (m *MailTM) CreateMailbox() (string, string, error) {
        // 1. Get available domain
        domain, err := m.getDomain()
        if err != nil {
                return "", "", fmt.Errorf("get domain: %w", err)
        }

        // 2. Generate random local part and password
        local := randomString(12, lowerAlnum)
        password := "MaritacaProxy@2024" + randomString(4, digits)
        email := local + "@" + domain

        // 3. Create account
        body := fmt.Sprintf(`{"address":"%s","password":"%s"}`, email, password)
        req, _ := http.NewRequest("POST", "https://api.mail.tm/accounts", strings.NewReader(body))
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("User-Agent", "Mozilla/5.0")

        resp, err := m.client.Do(req)
        if err != nil {
                return "", "", fmt.Errorf("create account: %w", err)
        }
        defer resp.Body.Close()
        respBody, _ := io.ReadAll(resp.Body)
        if resp.StatusCode >= 400 {
                return "", "", fmt.Errorf("mail.tm create account failed (%d): %s", resp.StatusCode, string(respBody))
        }

        // 4. Get auth token
        tokenBody := fmt.Sprintf(`{"address":"%s","password":"%s"}`, email, password)
        treq, _ := http.NewRequest("POST", "https://api.mail.tm/token", strings.NewReader(tokenBody))
        treq.Header.Set("Content-Type", "application/json")
        treq.Header.Set("User-Agent", "Mozilla/5.0")
        tresp, err := m.client.Do(treq)
        if err != nil {
                return email, password, fmt.Errorf("get token: %w", err)
        }
        defer tresp.Body.Close()
        trespBody, _ := io.ReadAll(tresp.Body)
        if tresp.StatusCode >= 400 {
                return email, password, fmt.Errorf("mail.tm get token failed (%d): %s", tresp.StatusCode, string(trespBody))
        }

        var tok struct {
                Token string `json:"token"`
        }
        if err := json.Unmarshal(trespBody, &tok); err != nil {
                return email, password, fmt.Errorf("parse token: %w", err)
        }
        m.token = tok.Token
        logger.Info("[tempmail] Created mailbox: %s", email)
        return email, password, nil
}

func (m *MailTM) getDomain() (string, error) {
        resp, err := m.client.Get("https://api.mail.tm/domains?page=1")
        if err != nil {
                return "", err
        }
        defer resp.Body.Close()
        var data struct {
                Members []struct {
                        Domain string `json:"domain"`
                } `json:"hydra:member"`
        }
        if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
                return "", err
        }
        if len(data.Members) == 0 {
                return "", fmt.Errorf("no domains available")
        }
        return data.Members[0].Domain, nil
}

func (m *MailTM) WaitForVerification(email, password string, timeout time.Duration) (string, error) {
        if m.token == "" {
                // Re-login
                body := fmt.Sprintf(`{"address":"%s","password":"%s"}`, email, password)
                req, _ := http.NewRequest("POST", "https://api.mail.tm/token", strings.NewReader(body))
                req.Header.Set("Content-Type", "application/json")
                resp, err := m.client.Do(req)
                if err != nil {
                        return "", err
                }
                var tok struct{ Token string `json:"token"` }
                json.NewDecoder(resp.Body).Decode(&tok)
                resp.Body.Close()
                m.token = tok.Token
        }

        deadline := time.Now().Add(timeout)
        startTime := time.Now()
        pollInterval := 3 * time.Second
        processedMsgs := map[string]bool{}
        pollCount := 0

        for time.Now().Before(deadline) {
                pollCount++
                req, _ := http.NewRequest("GET", "https://api.mail.tm/messages?page=1", nil)
                req.Header.Set("Authorization", "Bearer "+m.token)
                req.Header.Set("User-Agent", "Mozilla/5.0")
                resp, err := m.client.Do(req)
                if err != nil {
                        logger.Warn("[tempmail] Poll #%d failed: %v", pollCount, err)
                        time.Sleep(pollInterval)
                        continue
                }
                var data struct {
                        Members []struct {
                                ID      string `json:"id"`
                                From    struct {
                                        Address string `json:"address"`
                                        Name    string `json:"name"`
                                } `json:"from"`
                                Subject string `json:"subject"`
                        } `json:"hydra:member"`
                }
                json.NewDecoder(resp.Body).Decode(&data)
                resp.Body.Close()

                elapsed := time.Since(startTime).Round(time.Second)
                logger.Info("[tempmail] Poll #%d (%v elapsed): %d message(s) in %s", pollCount, elapsed, len(data.Members), email)

                for _, msg := range data.Members {
                        if processedMsgs[msg.ID] {
                                continue
                        }
                        fromAddr := strings.ToLower(msg.From.Address + " " + msg.From.Name)
                        subj := strings.ToLower(msg.Subject)
                        if strings.Contains(fromAddr, "maritaca") ||
                                strings.Contains(fromAddr, "auth0") ||
                                strings.Contains(subj, "verif") ||
                                strings.Contains(subj, "confirm") {

                                logger.Info("[tempmail] Found verification email from=%s subj=%s", msg.From.Address, msg.Subject)
                                processedMsgs[msg.ID] = true

                                // Get full message
                                breq, _ := http.NewRequest("GET", "https://api.mail.tm/messages/"+msg.ID, nil)
                                breq.Header.Set("Authorization", "Bearer "+m.token)
                                breq.Header.Set("User-Agent", "Mozilla/5.0")
                                bresp, err := m.client.Do(breq)
                                if err != nil {
                                        continue
                                }
                                var full struct {
                                        HTML json.RawMessage `json:"html"`
                                        Text json.RawMessage `json:"text"`
                                }
                                json.NewDecoder(bresp.Body).Decode(&full)
                                bresp.Body.Close()

                                content := rawMessageToString(full.HTML) + "\n" + rawMessageToString(full.Text)
                                if url := extractVerificationURL(content); url != "" {
                                        return url, nil
                                }
                                if code := extractOTP(content); code != "" {
                                        return code, nil
                                }
                                logger.Warn("[tempmail] Email found but no verification URL/OTP extracted")
                        }
                }
                time.Sleep(pollInterval)
        }
        return "", fmt.Errorf("timeout waiting for verification email on %s", email)
}

func (m *MailTM) Close() error { return nil }

// ─── OneSecMail ─────────────────────────────────────────────────────────────

// OneSecMail uses the free 1secmail.com API.
type OneSecMail struct {
        domain  string
        client  *http.Client
        baseURL string
}

func NewOneSecMail(domain string) *OneSecMail {
        return &OneSecMail{
                domain:  domain,
                client:  &http.Client{Timeout: 15 * time.Second},
                baseURL: "https://www.1secmail.com/api/v1",
        }
}

func (m *OneSecMail) randomDomain() string {
        if m.domain != "" {
                return m.domain
        }
        domains := []string{"1secmail.com", "1secmail.net", "1secmail.org", "esiix.com", "wwjmp.com", "xojxe.com"}
        return domains[int(time.Now().UnixNano())%len(domains)]
}

func (m *OneSecMail) CreateMailbox() (string, string, error) {
        domain := m.randomDomain()
        local := randomString(12, lowerAlnum)
        email := local + "@" + domain
        logger.Info("[tempmail] Created mailbox: %s", email)
        return email, "", nil
}

func (m *OneSecMail) WaitForVerification(email, password string, timeout time.Duration) (string, error) {
        parts := strings.SplitN(email, "@", 2)
        if len(parts) != 2 {
                return "", fmt.Errorf("invalid email format: %s", email)
        }
        local, domain := parts[0], parts[1]

        deadline := time.Now().Add(timeout)
        pollInterval := 5 * time.Second

        for time.Now().Before(deadline) {
                url := fmt.Sprintf("%s/?action=getMessages&login=%s&domain=%s", m.baseURL, local, domain)
                req, _ := http.NewRequest("GET", url, nil)
                req.Header.Set("User-Agent", "Mozilla/5.0")
                resp, err := m.client.Do(req)
                if err != nil {
                        logger.Warn("[tempmail] Poll error: %v", err)
                        time.Sleep(pollInterval)
                        continue
                }
                body, _ := io.ReadAll(resp.Body)
                resp.Body.Close()

                var msgs []struct {
                        ID   int    `json:"id"`
                        From string `json:"from"`
                        Subj string `json:"subject"`
                }
                if err := json.Unmarshal(body, &msgs); err != nil {
                        time.Sleep(pollInterval)
                        continue
                }

                for _, msg := range msgs {
                        if strings.Contains(strings.ToLower(msg.From), "maritaca") ||
                                strings.Contains(strings.ToLower(msg.From), "auth0") ||
                                strings.Contains(strings.ToLower(msg.Subj), "verif") ||
                                strings.Contains(strings.ToLower(msg.Subj), "confirm") {

                                bodyURL := fmt.Sprintf("%s/?action=readMessage&login=%s&domain=%s&id=%d", m.baseURL, local, domain, msg.ID)
                                breq, _ := http.NewRequest("GET", bodyURL, nil)
                                breq.Header.Set("User-Agent", "Mozilla/5.0")
                                bresp, err := m.client.Do(breq)
                                if err != nil {
                                        continue
                                }
                                bbody, _ := io.ReadAll(bresp.Body)
                                bresp.Body.Close()

                                var full struct {
                                        HTML string `json:"htmlBody"`
                                        Text string `json:"textBody"`
                                        Body string `json:"body"`
                                }
                                if err := json.Unmarshal(bbody, &full); err == nil {
                                        content := full.HTML + "\n" + full.Text + "\n" + full.Body
                                        if url := extractVerificationURL(content); url != "" {
                                                return url, nil
                                        }
                                        if code := extractOTP(content); code != "" {
                                                return code, nil
                                        }
                                }
                        }
                }
                time.Sleep(pollInterval)
        }
        return "", fmt.Errorf("timeout waiting for verification email on %s", email)
}

func (m *OneSecMail) Close() error { return nil }

// ─── GuerrillaMail ──────────────────────────────────────────────────────────

type GuerrillaMail struct {
        client *http.Client
        sid    string
}

func NewGuerrillaMail() *GuerrillaMail {
        return &GuerrillaMail{
                client: &http.Client{Timeout: 15 * time.Second},
        }
}

func (m *GuerrillaMail) CreateMailbox() (string, string, error) {
        resp, err := m.client.Get("https://api.guerrillamail.com/ajax.php?f=get_email_address&lang=en")
        if err != nil {
                return "", "", err
        }
        defer resp.Body.Close()
        var data struct {
                EmailAddr string `json:"email_addr"`
                SIDToken  string `json:"sid_token"`
        }
        if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
                return "", "", err
        }
        m.sid = data.SIDToken
        return data.EmailAddr, "", nil
}

func (m *GuerrillaMail) WaitForVerification(email, password string, timeout time.Duration) (string, error) {
        deadline := time.Now().Add(timeout)
        for time.Now().Before(deadline) {
                url := fmt.Sprintf("https://api.guerrillamail.com/ajax.php?f=get_email_list&offset=0&sid_token=%s", m.sid)
                resp, err := m.client.Get(url)
                if err != nil {
                        time.Sleep(5 * time.Second)
                        continue
                }
                var data struct {
                        List []struct {
                                MailID   int    `json:"mail_id"`
                                MailFrom string `json:"mail_from"`
                                MailSubj string `json:"mail_subject"`
                        } `json:"list"`
                }
                json.NewDecoder(resp.Body).Decode(&data)
                resp.Body.Close()

                for _, msg := range data.List {
                        if strings.Contains(strings.ToLower(msg.MailFrom), "maritaca") ||
                                strings.Contains(strings.ToLower(msg.MailFrom), "auth0") ||
                                strings.Contains(strings.ToLower(msg.MailSubj), "verif") {

                                bodyURL := fmt.Sprintf("https://api.guerrillamail.com/ajax.php?f=fetch_email&email_id=mail_%d&sid_token=%s", msg.MailID, m.sid)
                                bresp, err := m.client.Get(bodyURL)
                                if err != nil {
                                        continue
                                }
                                var bdata struct {
                                        MailBody string `json:"mail_body"`
                                }
                                json.NewDecoder(bresp.Body).Decode(&bdata)
                                bresp.Body.Close()
                                if url := extractVerificationURL(bdata.MailBody); url != "" {
                                        return url, nil
                                }
                                if code := extractOTP(bdata.MailBody); code != "" {
                                        return code, nil
                                }
                        }
                }
                time.Sleep(5 * time.Second)
        }
        return "", fmt.Errorf("timeout waiting for verification email")
}

func (m *GuerrillaMail) Close() error { return nil }

// ─── Helpers ───────────────────────────────────────────────────────────────

var (
        urlRE         = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
        otpRE         = regexp.MustCompile(`(?i)\b(\d{6})\b`)
        // Auth0 verification URL (the actual verification endpoint)
        // e.g. https://auth.maritaca.ai/u/email-verification?ticket=...#
        auth0VerifyRE = regexp.MustCompile(`(?i)https?://auth\.maritaca\.ai/u/email-verification\?ticket=[A-Za-z0-9_-]+#?`)
        // Link element: href="tracker_url">visible_text (https://auth.maritaca.ai/u/email-verification?ticket=...)
        linkWithAuth0TextRE = regexp.MustCompile(`(?is)href="[^"]*"[^>]*>\s*(https?://auth\.maritaca\.ai/u/email-verification\?ticket=[A-Za-z0-9_-]+#?)\s*</a>`)
        // Fallback for older format
        maritacaVerifyRE = regexp.MustCompile(`(?i)https?://[^\s"'<>]*chat\.maritaca\.ai[^\s"'<>]*(?:verify|email|confirm)[^\s"'<>]*`)
        genericVerifyRE  = regexp.MustCompile(`(?i)https?://[^\s"'<>]*(?:verify|email-verification|ticket=)[^\s"'<>]*`)
        trackerRE     = regexp.MustCompile(`(?i)https?://url\d+\.maritaca\.ai/[^\s"'<>]+`)
)

func extractVerificationURL(content string) string {
        // 1) Try to find an Auth0 verification URL visible inside an <a>...</a> tag
        // (Maritaca wraps the actual URL as text inside a link whose href is a tracker)
        if m := linkWithAuth0TextRE.FindStringSubmatch(content); len(m) > 1 {
                return m[1]
        }
        // 2) Try direct Auth0 verification URL
        if m := auth0VerifyRE.FindString(content); m != "" {
                return strings.TrimRight(m, ".,;)")
        }
        // 3) Then Maritaca chat URLs with verify
        if m := maritacaVerifyRE.FindString(content); m != "" {
                return strings.TrimRight(m, ".,;)")
        }
        // 4) Then generic URLs containing verify/ticket keywords (but skip tracker URLs)
        allURLs := urlRE.FindAllString(content, -1)
        for _, u := range allURLs {
                if trackerRE.MatchString(u) {
                        continue
                }
                if genericVerifyRE.MatchString(u) {
                        return strings.TrimRight(u, ".,;)")
                }
        }
        return ""
}

func extractOTP(content string) string {
        if m := otpRE.FindString(content); m != "" {
                return m
        }
        return ""
}

// rawMessageToString converts a json.RawMessage that may be a string or an
// array of strings into a single concatenated string. Mail.tm sometimes
// returns html/text fields as arrays.
func rawMessageToString(raw json.RawMessage) string {
        if len(raw) == 0 {
                return ""
        }
        // Try as string
        var s string
        if err := json.Unmarshal(raw, &s); err == nil {
                return s
        }
        // Try as array of strings
        var arr []string
        if err := json.Unmarshal(raw, &arr); err == nil {
                return strings.Join(arr, "")
        }
        // Try as array of any
        var arrAny []interface{}
        if err := json.Unmarshal(raw, &arrAny); err == nil {
                var parts []string
                for _, item := range arrAny {
                        if str, ok := item.(string); ok {
                                parts = append(parts, str)
                        }
                }
                return strings.Join(parts, "")
        }
        return string(raw)
}

const (
        lowerAlnum = "abcdefghijklmnopqrstuvwxyz0123456789"
        digits     = "0123456789"
)

func randomString(n int, charset string) string {
        r := rand.New(rand.NewSource(time.Now().UnixNano()))
        b := make([]byte, n)
        for i := range b {
                b[i] = charset[r.Intn(len(charset))]
        }
        return string(b)
}
