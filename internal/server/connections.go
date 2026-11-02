// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"amplio/internal/config"
)

// The settings API is intentionally optional: non-OpenAI Amplio setups are not
// changed. No secret is put in config.toml, run metadata, or response JSON.
// The subprocess bridge reads the key from its own owner-only file on demand.
const (
	maxAPIKeyBytes    = 4096
	loginStartTimeout = 10 * time.Second
	loginLifetime     = 200 * time.Second
)

var loginState = struct {
	sync.Mutex
	pending bool
}{}

func openAIKeyFile() string {
	if path := os.Getenv("AMPLIO_OPENAI_KEY_FILE"); path != "" {
		return path
	}
	return filepath.Join(config.DataDir(), "credentials", "openai_api_key")
}

func siwcHome() string {
	if path := os.Getenv("AMPLIO_SIWC_HOME"); path != "" {
		return path
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "amplio-chatgpt", "siwc")
}

// bridgePath looks relative to the installed binary and the working directory.
// An explicit environment path permits deployed binaries with separate assets.
func bridgePath() (string, error) {
	candidates := []string{}
	if explicit := os.Getenv("AMPLIO_OPENAI_BRIDGE"); explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(exe), "bridges", "openai_responses.py"))
		}
		candidates = append(candidates, filepath.Join("bridges", "openai_responses.py"))
	}
	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err == nil && info.Mode().IsRegular() {
			return abs, nil
		}
	}
	return "", errors.New("OpenAI bridge not found; install alongside Amplio or set AMPLIO_OPENAI_BRIDGE")
}

func loginScript() (string, error) {
	if v := os.Getenv("AMPLIO_OPENAI_LOGIN_SCRIPT"); v != "" {
		if p, err := filepath.Abs(v); err == nil {
			return p, nil
		}
	}
	bridge, err := bridgePath()
	if err != nil {
		return "", err
	}
	script := filepath.Join(filepath.Dir(filepath.Dir(bridge)), "scripts", "login_chatgpt.py")
	if info, err := os.Stat(script); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("ChatGPT login helper not found beside the bridge")
	}
	return script, nil
}

func privateRegular(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return false, errors.New("credential file must be a regular owner-only file (0600)")
	}
	return true, nil
}

// Treat API billing explicitly; a preexisting environment key always wins in
// the bridge, so saving a second key through the UI would be misleading.
func keySource() (string, error) {
	if os.Getenv("OPENAI_API_KEY") != "" {
		return "environment", nil
	}
	exists, err := privateRegular(openAIKeyFile())
	if err != nil {
		return "", err
	}
	if exists {
		return "stored", nil
	}
	return "none", nil
}

func chatGPTState() (connected bool, email string, err error) {
	path := filepath.Join(siwcHome(), "account.json")
	exists, err := privateRegular(path)
	if err != nil || !exists {
		return false, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "", err
	}
	if len(data) > 128*1024 {
		return false, "", errors.New("credential record is unexpectedly large")
	}
	var grant struct {
		ClientID     string   `json:"client_id"`
		Subject      string   `json:"subject"`
		Email        string   `json:"email"`
		AccessToken  string   `json:"access_token"`
		RefreshToken string   `json:"refresh_token"`
		Scopes       []string `json:"scopes"`
	}
	if json.Unmarshal(data, &grant) != nil {
		return false, "", errors.New("invalid ChatGPT credential record")
	}
	for _, scope := range grant.Scopes {
		if scope == "chatgpt.tokens.use.direct" && grant.ClientID != "" && grant.Subject != "" && grant.AccessToken != "" && grant.RefreshToken != "" {
			return true, grant.Email, nil
		}
	}
	return false, grant.Email, nil
}

func privateTransport(r *http.Request) bool {
	// Local HTTP is acceptable. Remote credentials require HTTPS all the way to
	// this server, or a trusted loopback reverse proxy / SSH tunnel.
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func ownerOnlyOrigin(r *http.Request) bool {
	if !privateTransport(r) {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
	}
	if fetchSite := r.Header.Get("Sec-Fetch-Site"); fetchSite != "" && fetchSite != "same-origin" && fetchSite != "none" {
		return false
	}
	return true
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.token == "" {
		writeErr(w, 403, "credential management requires an authenticated Amplio server")
		return
	}
	key, err := keySource()
	if err != nil {
		writeErr(w, 500, "API credential file permissions invalid")
		return
	}
	chat, email, err := chatGPTState()
	if err != nil {
		writeErr(w, 500, "ChatGPT credential file permissions or format invalid")
		return
	}
	loginState.Lock()
	pending := loginState.pending
	loginState.Unlock()
	bridge, bridgeErr := bridgePath()
	if bridgeErr != nil {
		bridge = ""
	}
	writeJSON(w, 200, map[string]any{
		"api_key_source":    key,
		"chatgpt_connected": chat,
		"chatgpt_email":     email,
		"login_pending":     pending,
		"bridge_path":       bridge,
	})
}

func (s *Server) handleSetAPIKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.token == "" {
		writeErr(w, 403, "credential management requires an authenticated Amplio server")
		return
	}
	if !ownerOnlyOrigin(r) {
		writeErr(w, 403, "cross-origin credential change denied")
		return
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		writeErr(w, 409, "OPENAI_API_KEY is already set by the server environment; change it there")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIKeyBytes+256)
	var payload struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeErr(w, 400, "expected JSON with key")
		return
	}
	key := strings.TrimSpace(payload.Key)
	if len(key) < 8 || len(key) > maxAPIKeyBytes || strings.ContainsAny(key, "\r\n\t ") {
		writeErr(w, 400, "invalid API key format")
		return
	}
	path := openAIKeyFile()
	if err := writePrivateKey(path, key); err != nil {
		writeErr(w, 500, "cannot save API key securely")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func writePrivateKey(path, key string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	d, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !d.IsDir() || d.Mode().Perm()&0o077 != 0 {
		return errors.New("credential directory is not private")
	}
	if _, err := privateRegular(path); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".openai-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.WriteString(key + "\n"); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.token == "" {
		writeErr(w, 403, "credential management requires an authenticated Amplio server")
		return
	}
	if !ownerOnlyOrigin(r) {
		writeErr(w, 403, "cross-origin credential change denied")
		return
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		writeErr(w, 409, "key is managed by the server environment")
		return
	}
	path := openAIKeyFile()
	exists, err := privateRegular(path)
	if err != nil {
		writeErr(w, 500, "invalid API credential file")
		return
	}
	if exists {
		if err := os.Remove(path); err != nil {
			writeErr(w, 500, "cannot remove API credential file")
			return
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Disconnect only removes the local authorization grant. It does not claim to
// revoke a credential at OpenAI; the operator may revoke it in account settings.
func (s *Server) handleDisconnectChatGPT(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.token == "" {
		writeErr(w, 403, "credential management requires an authenticated Amplio server")
		return
	}
	if !ownerOnlyOrigin(r) {
		writeErr(w, 403, "cross-origin logout denied")
		return
	}
	path := filepath.Join(siwcHome(), "account.json")
	exists, err := privateRegular(path)
	if err != nil {
		writeErr(w, 500, "ChatGPT credential file permissions invalid")
		return
	}
	if exists {
		if err := os.Remove(path); err != nil {
			writeErr(w, 500, "cannot remove ChatGPT credentials")
			return
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// A login is started from an authenticated click. The Python helper runs a
// temporary loopback OAuth callback listener; only the URL, never the OAuth
// tokens or callback code, is returned to the browser. This requires a local
// browser (or explicit port forwarding of the ephemeral loopback port).
var safeLoginURL = regexp.MustCompile(`^https://auth\.openai\.com/`)

func (s *Server) handleBeginChatGPTLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.token == "" {
		writeErr(w, 403, "credential management requires an authenticated Amplio server")
		return
	}
	if !ownerOnlyOrigin(r) {
		writeErr(w, 403, "cross-origin login denied")
		return
	}
	script, err := loginScript()
	if err != nil {
		writeErr(w, 501, "ChatGPT login helper unavailable; see setup guide")
		return
	}
	// Fail before opening the browser if the optional authentication verifier
	// is absent. This probe never receives credentials.
	if err := exec.Command("python3", "-c", "import jwt; import cryptography").Run(); err != nil {
		writeErr(w, 501, "install PyJWT[crypto] to use browser login")
		return
	}
	loginState.Lock()
	if loginState.pending {
		loginState.Unlock()
		writeErr(w, 409, "ChatGPT login already in progress")
		return
	}
	loginState.pending = true
	loginState.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), loginLifetime)
	cmd := exec.CommandContext(ctx, "python3", script, "--headless")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		clearPendingLogin()
		writeErr(w, 500, "could not prepare login process")
		return
	}
	cmd.Stderr = io.Discard // No tokens/authorization parameters are logged.
	if err := cmd.Start(); err != nil {
		cancel()
		clearPendingLogin()
		writeErr(w, 500, "could not start login process")
		return
	}
	first := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			first <- scanner.Text()
		} else {
			first <- ""
		}
		for scanner.Scan() {
		} // drain a short completion line without logging it
		_ = cmd.Wait()
		cancel()
		clearPendingLogin()
	}()
	timer := time.NewTimer(loginStartTimeout)
	defer timer.Stop()
	var line string
	select {
	case line = <-first:
	case <-timer.C:
		_ = cmd.Process.Kill()
		writeErr(w, 504, "ChatGPT login did not start")
		return
	}
	var msg struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if json.Unmarshal([]byte(line), &msg) != nil || !safeLoginURL.MatchString(msg.AuthorizationURL) {
		_ = cmd.Process.Kill()
		writeErr(w, 502, "ChatGPT login helper returned no authorization URL")
		return
	}
	writeJSON(w, 200, map[string]string{"authorization_url": msg.AuthorizationURL})
}

func clearPendingLogin() { loginState.Lock(); loginState.pending = false; loginState.Unlock() }

// Explicitly called from serve before constructing any LLMs. This only
// exports the FILE PATH, not its contents, to bridge subprocesses.
func ConfigureOpenAIKeyFile(dataDir string) error {
	if os.Getenv("AMPLIO_OPENAI_KEY_FILE") != "" {
		return nil
	}
	return os.Setenv("AMPLIO_OPENAI_KEY_FILE", filepath.Join(dataDir, "credentials", "openai_api_key"))
}
