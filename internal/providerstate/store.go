// SPDX-License-Identifier: Apache-2.0
// Package providerstate owns local provider settings and credentials. It never
// runs a helper binary or changes the selected billing method implicitly.
package providerstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrNotConfigured = errors.New("finish setup in Providers before starting a run")

type Settings struct {
	Agent string `json:"agent"`
	HQ    string `json:"hq"`
	Fast  string `json:"fast"`
}

func (s Settings) Ready() bool { return s.Agent != "" && s.HQ != "" && s.Fast != "" }
func (s Settings) For(role string) string {
	switch role {
	case "agent":
		return s.Agent
	case "hq":
		return s.HQ
	case "fast":
		return s.Fast
	}
	return ""
}

type account struct {
	Subject      string    `json:"subject"`
	Email        string    `json:"email"`
	ClientID     string    `json:"client_id"`
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Scopes       []string  `json:"scopes"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (a account) permitted() bool {
	return has(a.Scopes, "chatgpt.tokens.use.direct") && has(a.Scopes, "resource.invoke")
}
func has(a []string, v string) bool {
	for _, s := range a {
		if s == v {
			return true
		}
	}
	return false
}

type Status struct {
	APIKeySource     string    `json:"api_key_source"`
	ChatGPTSignedIn  bool      `json:"chatgpt_signed_in"`
	ChatGPTConnected bool      `json:"chatgpt_connected"`
	ChatGPTEmail     string    `json:"chatgpt_email,omitempty"`
	LoginState       string    `json:"login_state"`
	LoginError       string    `json:"login_error,omitempty"`
	LoginPending     bool      `json:"login_pending"`
	LoginExpiresAt   time.Time `json:"login_expires_at,omitempty"`
	SetupRequired    bool      `json:"setup_required"`
	Settings         Settings  `json:"settings"`
}

// Amplio's data-directory process lock prevents two servers using this store.
// Native tokens deliberately do not share a rotating legacy Python/Codex grant.
type Manager struct {
	dir          string
	mu           sync.Mutex
	fallback     Settings
	epoch        uint64
	loginMu      sync.Mutex
	pending      *attempt
	loginState   string
	loginError   string
	client       *http.Client
	authorizeURL string
	tokenURL     string
	jwksURL      string
	apiURL       string
	now          func() time.Time
}

var managers sync.Map

func ForDir(dir string) *Manager {
	dir, _ = filepath.Abs(dir)
	m := newManager(dir)
	actual, _ := managers.LoadOrStore(dir, m)
	return actual.(*Manager)
}
func newManager(dir string) *Manager {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	return &Manager{dir: dir, client: &http.Client{Timeout: 30 * time.Second, Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		authorizeURL: "https://auth.openai.com/api/accounts/authorize", tokenURL: "https://auth.openai.com/api/accounts/oauth/token", jwksURL: "https://auth.openai.com/.well-known/jwks.json", apiURL: "https://api.openai.com/v1", now: time.Now, loginState: "idle"}
}
func (m *Manager) credentials() string { return filepath.Join(m.dir, "credentials") }
func (m *Manager) accountPath() string { return filepath.Join(m.credentials(), "chatgpt.json") }
func (m *Manager) keyPath() string {
	if p := os.Getenv("AMPLIO_OPENAI_KEY_FILE"); p != "" {
		return p
	}
	return filepath.Join(m.credentials(), "openai_api_key")
}
func (m *Manager) SetFallback(s Settings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range []*string{&s.Agent, &s.HQ, &s.Fast} {
		if strings.HasPrefix(*p, "managed:") {
			*p = ""
		}
	}
	m.fallback = s
}
func (m *Manager) settingsLocked() (Settings, error) {
	s := m.fallback
	b, err := readPrivate(filepath.Join(m.dir, "providers.json"), 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return Settings{}, err
	}
	var saved Settings
	if err = json.Unmarshal(b, &saved); err != nil {
		return Settings{}, errors.New("invalid saved provider settings; restore providers.json rather than changing billing implicitly")
	}
	if saved.Agent != "" {
		s.Agent = saved.Agent
	}
	if saved.HQ != "" {
		s.HQ = saved.HQ
	}
	if saved.Fast != "" {
		s.Fast = saved.Fast
	}
	return s, nil
}
func (m *Manager) Settings() (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settingsLocked()
}
func (m *Manager) Selected(role string) (string, error) {
	s, err := m.Settings()
	if err != nil {
		return "", err
	}
	v := s.For(role)
	if v == "" {
		return "", ErrNotConfigured
	}
	if strings.HasPrefix(v, "managed:") {
		return "", errors.New("recursive managed model selection")
	}
	return v, nil
}
func (m *Manager) SaveSettings(s Settings) error {
	if !s.Ready() {
		return ErrNotConfigured
	}
	for _, v := range []string{s.Agent, s.HQ, s.Fast} {
		if len(v) > 2048 || strings.ContainsAny(v, "\r\n") || strings.HasPrefix(v, "managed:") {
			return errors.New("invalid model selection")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return writeJSONPrivate(filepath.Join(m.dir, "providers.json"), s, false)
}
func (m *Manager) accountLocked() (account, error) {
	var a account
	b, err := readPrivate(m.accountPath(), 128<<10)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	if json.Unmarshal(b, &a) != nil {
		return a, errors.New("invalid ChatGPT credential file")
	}
	return a, nil
}
func (m *Manager) APIKey() (string, error) {
	if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
		return key, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := readPrivate(m.keyPath(), 4097)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("add an OpenAI API key in Providers")
	}
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", errors.New("stored API key is empty")
	}
	return key, nil
}
func (m *Manager) SetAPIKey(key string) error {
	if os.Getenv("OPENAI_API_KEY") != "" {
		return errors.New("API key is managed by OPENAI_API_KEY; the UI cannot overwrite it")
	}
	key = strings.TrimSpace(key)
	if len(key) < 8 || len(key) > 4096 || strings.ContainsAny(key, " \r\n\t") {
		return errors.New("invalid API key format")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return writePrivate(m.keyPath(), []byte(key+"\n"), true)
}
func (m *Manager) DeleteAPIKey() error {
	if os.Getenv("OPENAI_API_KEY") != "" {
		return errors.New("API key is managed by OPENAI_API_KEY")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return removePrivate(m.keyPath())
}
func (m *Manager) Disconnect() error {
	m.CancelLogin()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epoch++
	return removePrivate(m.accountPath())
}
func (m *Manager) Status() (Status, error) {
	var s Status
	m.mu.Lock()
	settings, err := m.settingsLocked()
	if err != nil {
		m.mu.Unlock()
		return s, err
	}
	a, err := m.accountLocked()
	if err != nil {
		m.mu.Unlock()
		return s, err
	}
	s.Settings = settings
	s.SetupRequired = !settings.Ready()
	s.ChatGPTSignedIn = a.Subject != ""
	s.ChatGPTConnected = a.permitted() && a.RefreshToken != ""
	s.ChatGPTEmail = a.Email
	s.APIKeySource = "none"
	if os.Getenv("OPENAI_API_KEY") != "" {
		s.APIKeySource = "environment"
	} else {
		b, e := readPrivate(m.keyPath(), 4097)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			m.mu.Unlock()
			return s, e
		}
		if len(strings.TrimSpace(string(b))) > 0 {
			s.APIKeySource = "stored"
		}
	}
	m.mu.Unlock()
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	s.LoginState = m.loginState
	s.LoginError = m.loginError
	s.LoginPending = m.pending != nil
	if m.pending != nil {
		s.LoginExpiresAt = m.pending.expires
	}
	return s, nil
}

// Reject symlinks, public files and oversized documents. Comparing identities
// after open closes the final-component replacement race.
func readPrivate(path string, limit int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, errors.New("credential/settings file must be regular and owner-only (0600)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New("credential/settings file changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("credential/settings file is too large")
	}
	return b, nil
}
func writeJSONPrivate(path string, v any, privateDir bool) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(b, '\n'), privateDir)
}
func writePrivate(path string, b []byte, privateDir bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (privateDir && info.Mode().Perm()&0077 != 0) {
		return errors.New("credential directory must be private (0700), not a symlink")
	}
	if _, err = os.Lstat(path); err == nil {
		if _, err = readPrivate(path, 1<<20); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(dir, ".provider-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
func removePrivate(path string) error {
	_, err := readPrivate(path, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil {
		return fmt.Errorf("remove local credential: %w", err)
	}
	return nil
}
