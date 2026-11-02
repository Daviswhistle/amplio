// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func connectionRequest(t *testing.T, srv *Server, method, path, body, auth, origin, remote string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if remote != "" {
		r.RemoteAddr = remote
	} else {
		r.RemoteAddr = "127.0.0.1:55555"
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

func TestConnectionsKeyLifecycleAndAccess(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	keyFile := filepath.Join(t.TempDir(), "credentials", "openai_api_key")
	t.Setenv("AMPLIO_OPENAI_KEY_FILE", keyFile)
	t.Setenv("AMPLIO_SIWC_HOME", filepath.Join(t.TempDir(), "siwc"))
	srv := &Server{token: "secret"}
	if rr := connectionRequest(t, srv, "GET", "/api/connections", "", "", "", ""); rr.Code != 401 {
		t.Fatalf("status without owner token: %d", rr.Code)
	}
	if rr := connectionRequest(t, srv, "POST", "/api/connections/api-key", `{"key":"sk-good-key-123"}`, "", "", ""); rr.Code != 401 {
		t.Fatalf("write without owner token: %d", rr.Code)
	}
	if rr := connectionRequest(t, srv, "POST", "/api/connections/api-key", `{"key":"sk-good-key-123"}`, "secret", "https://elsewhere.test", ""); rr.Code != 403 {
		t.Fatalf("cross-origin accepted: %d", rr.Code)
	}
	if rr := connectionRequest(t, srv, "POST", "/api/connections/api-key", `{"key":"sk-good-key-123"}`, "secret", "", "198.51.100.10:2222"); rr.Code != 403 {
		t.Fatalf("insecure remote transport accepted: %d", rr.Code)
	}
	if rr := connectionRequest(t, srv, "POST", "/api/connections/api-key", `{"key":"sk-good-key-123"}`, "secret", "", ""); rr.Code != 200 {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("credential mode %o, want 600", info.Mode().Perm())
	}
	status := connectionRequest(t, srv, "GET", "/api/connections", "", "secret", "", "")
	if status.Code != 200 || strings.Contains(status.Body.String(), "sk-good-key-123") {
		t.Fatalf("status leaked secret or failed: %d %s", status.Code, status.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["api_key_source"] != "stored" {
		t.Fatalf("api_key_source=%v", got["api_key_source"])
	}
	if rr := connectionRequest(t, srv, "DELETE", "/api/connections/api-key", "", "secret", "", ""); rr.Code != 200 {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body)
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatalf("key persisted after removal: %v", err)
	}
}

func TestConnectionsRefuseEnvOverrideAndSymlink(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-env-secret")
	t.Setenv("AMPLIO_OPENAI_KEY_FILE", filepath.Join(t.TempDir(), "key"))
	srv := &Server{token: "secret"}
	if rr := connectionRequest(t, srv, "POST", "/api/connections/api-key", `{"key":"sk-unexpected"}`, "secret", "", ""); rr.Code != 409 {
		t.Fatalf("environment key override: %d", rr.Code)
	}
	if rr := connectionRequest(t, srv, "DELETE", "/api/connections/api-key", "", "secret", "", ""); rr.Code != 409 {
		t.Fatalf("environment key deletion: %d", rr.Code)
	}
	t.Setenv("OPENAI_API_KEY", "")
	original := filepath.Join(t.TempDir(), "original")
	if err := os.WriteFile(original, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	keyFile := os.Getenv("AMPLIO_OPENAI_KEY_FILE")
	if err := os.Symlink(original, keyFile); err != nil {
		t.Fatal(err)
	}
	if rr := connectionRequest(t, srv, "POST", "/api/connections/api-key", `{"key":"sk-unexpected"}`, "secret", "", ""); rr.Code != 500 {
		t.Fatalf("symlink overwrite: %d", rr.Code)
	}
	saved, err := os.ReadFile(original)
	if err != nil || string(saved) != "safe" {
		t.Fatal("symlink overwrite mutated target")
	}
}

func TestDisconnectChatGPTClearsOnlyOwnerFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AMPLIO_SIWC_HOME", dir)
	path := filepath.Join(dir, "account.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{token: "secret"}
	if rr := connectionRequest(t, srv, "DELETE", "/api/connections/chatgpt", "", "", "", ""); rr.Code != 401 {
		t.Fatalf("unauthed logout %d", rr.Code)
	}
	if rr := connectionRequest(t, srv, "DELETE", "/api/connections/chatgpt", "", "secret", "https://evil.test", ""); rr.Code != 403 {
		t.Fatalf("cross-origin logout %d", rr.Code)
	}
	if rr := connectionRequest(t, srv, "DELETE", "/api/connections/chatgpt", "", "secret", "", ""); rr.Code != 200 {
		t.Fatalf("logout %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("credential not removed %v", err)
	}
}

func TestConnectionsPrivateFileAndGrantStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")
	t.Setenv("AMPLIO_SIWC_HOME", dir)
	if err := os.WriteFile(path, []byte(`{"client_id":"oaiapp_1","subject":"u1","email":"test@example.com","access_token":"secret","refresh_token":"secret","scopes":["chatgpt.tokens.use.direct"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	yes, email, err := chatGPTState()
	if err != nil || !yes || email != "test@example.com" {
		t.Fatalf("grant status: %v %q %v", yes, email, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if yes, _, err := chatGPTState(); yes || err == nil {
		t.Fatalf("insecure OAuth record accepted: %v", err)
	}
}
