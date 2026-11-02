// SPDX-License-Identifier: Apache-2.0
package server

import (
	"amplio/internal/providerstate"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func providerRequest(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12400"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestProvidersOwnerAndOrigin(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	m := providerstate.ForDir(t.TempDir())
	defer m.Close()
	h := srv.ProviderHandler(m, nil)
	for _, path := range []string{"/api/providers", "/api/connections", "/api/providers/models?provider=openai"} {
		if w := providerRequest(h, "GET", path, "", ""); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	for _, tc := range []struct{ name, origin, remote string }{{"foreign origin", "https://evil.invalid", "127.0.0.1:12400"}, {"remote plaintext", "", "192.0.2.1:12400"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://localhost/api/providers/api-key", strings.NewReader(`{"key":"sk-unit-secret"}`))
			r.RemoteAddr = tc.remote
			r.Header.Set("Authorization", "Bearer secret")
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
	srv.token = ""
	if w := providerRequest(h, "GET", "/api/providers", "", ""); w.Code != 403 {
		t.Fatalf("open server must not manage credentials: %d", w.Code)
	}
}
func TestProvidersFirstRunSaveAndMenu(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("AMPLIO_OPENAI_KEY_FILE", "")
	srv, _, store := newTestServer(t)
	dir := t.TempDir()
	m := providerstate.ForDir(dir)
	defer m.Close()
	ready := 0
	h := srv.ProviderHandler(m, func() { ready++ })
	w := providerRequest(h, "GET", "/api/providers", "", "secret")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"setup_required":true`) {
		t.Fatalf("fresh setup: %d %s", w.Code, w.Body.String())
	}
	w = providerRequest(h, "POST", "/api/runs", `{"task":"do not run"}`, "secret")
	if w.Code != 409 {
		t.Fatalf("unconfigured run: %d", w.Code)
	}
	w = providerRequest(h, "POST", "/api/providers/api-key", `{"key":"sk-offline-only-secret"}`, "secret")
	if w.Code != 200 || strings.Contains(w.Body.String(), "sk-offline") {
		t.Fatalf("save key: %d %s", w.Code, w.Body.String())
	}
	keyFile := filepath.Join(dir, "credentials", "openai_api_key")
	info, err := os.Stat(keyFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions: %v", err)
	}
	body := `{"agent":{"provider":"openai","model":"test-agent","effort":""},"hq":{"provider":"openai","model":"test-review","effort":"high"},"fast":{"provider":"openai","model":"test-fast","effort":""}}`
	w = providerRequest(h, "PUT", "/api/providers/settings", body, "secret")
	if w.Code != 200 || ready != 1 {
		t.Fatalf("save settings: %d %s ready=%d", w.Code, w.Body.String(), ready)
	}
	settings, err := m.Settings()
	if err != nil || settings.Agent != "openai-responses:test-agent" || settings.HQ != "openai-responses:test-review?reasoning.effort=high" {
		t.Fatalf("settings: %+v %v", settings, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("setup must not require or rewrite config.toml")
	}
	w = providerRequest(h, "GET", "/api/models", "", "")
	var menu modelMenu
	if json.Unmarshal(w.Body.Bytes(), &menu) != nil || menu.Default != settings.Agent {
		t.Fatalf("menu: %s", w.Body.String())
	}
	models, err := store.ListCustomModels(context.Background())
	if err != nil || len(models) != 3 {
		t.Fatalf("custom menu %v %v", models, err)
	}
	w = providerRequest(h, "PUT", "/api/providers/settings", strings.Replace(body, "test-agent", "test?auth=chatgpt", 1), "secret")
	if w.Code != 400 {
		t.Fatal("model injection accepted")
	}
	after, _ := m.Settings()
	if after != settings {
		t.Fatal("invalid settings changed defaults")
	}
	w = providerRequest(h, "DELETE", "/api/providers/api-key", "", "secret")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatal("key not removed")
	}
}
func TestProvidersNativeLoginAndCancellation(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	m := providerstate.ForDir(t.TempDir())
	defer m.Close()
	h := srv.ProviderHandler(m, nil)
	w := providerRequest(h, "POST", "/api/providers/chatgpt/login", `{}`, "secret")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "auth.openai.com") || !strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatalf("native login: %d %s", w.Code, w.Body.String())
	}
	w = providerRequest(h, "POST", "/api/providers/chatgpt/login", `{}`, "secret")
	if w.Code != 409 {
		t.Fatal("concurrent login not blocked")
	}
	w = providerRequest(h, "POST", "/api/providers/chatgpt/callback", `{"url":"https://evil.invalid/callback?code=not-valid"}`, "secret")
	if w.Code != 400 {
		t.Fatal("foreign callback accepted")
	}
	w = providerRequest(h, "DELETE", "/api/providers/chatgpt/login", "", "secret")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"login_pending":true`) {
		t.Fatalf("cancel: %s", w.Body.String())
	}
}
func TestProvidersRequestValidation(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	m := providerstate.ForDir(t.TempDir())
	defer m.Close()
	h := srv.ProviderHandler(m, nil)
	for _, body := range []string{`{"key":"sk-secret","unexpected":true}`, `{"key":"sk-secret"}{}`, strings.Repeat("x", 40000)} {
		w := providerRequest(h, "POST", "/api/providers/api-key", body, "secret")
		if w.Code != 400 {
			t.Fatalf("invalid body accepted %d", w.Code)
		}
	}
}
func TestProviderStartRunSnapshotsConcreteSelection(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServer(t)
	m := providerstate.ForDir(t.TempDir())
	defer m.Close()
	if err := m.SaveSettings(providerstate.Settings{Agent: "openai-responses:first", HQ: "openai-responses:hq", Fast: "openai-responses:fast"}); err != nil {
		t.Fatal(err)
	}
	var used string
	p := &providerHTTP{server: srv, accounts: m, app: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			LLM string `json:"llm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		used = body.LLM
		w.WriteHeader(201)
	})}
	h := http.HandlerFunc(p.startRun)
	w := providerRequest(h, "POST", "/api/runs", `{"task":"test"}`, "secret")
	if w.Code != 201 || used != "openai-responses:first" {
		t.Fatalf("snapshot %d %q", w.Code, used)
	}
	_ = m.SaveSettings(providerstate.Settings{Agent: "openai-responses:second", HQ: "openai-responses:hq", Fast: "openai-responses:fast"})
	w = providerRequest(h, "POST", "/api/runs", `{"task":"test","llm":"openai-responses:first"}`, "secret")
	if w.Code != 201 || used != "openai-responses:first" {
		t.Fatal("explicit selection was changed")
	}
	w = providerRequest(h, "POST", "/api/runs", `{"task":"test","llm":"managed:hq"}`, "secret")
	if w.Code != 400 {
		t.Fatal("mutable agent selection accepted")
	}
}
