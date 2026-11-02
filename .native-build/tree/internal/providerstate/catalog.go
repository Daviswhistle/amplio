// SPDX-License-Identifier: Apache-2.0
package providerstate

import (
 "bytes"
 "context"
 "encoding/json"
 "errors"
 "io"
 "net/http"
 "os"
 "sort"
 "strings"
 "time"
)

type Model struct { ID string `json:"id"`; Name string `json:"name"` }

func (m *Manager) Request(ctx context.Context, mode, method, path string, body []byte) (*http.Response, error) {
 if path != "/models" && path != "/responses" { return nil, errors.New("unsupported OpenAI route") }
 token := ""
 var err error
 switch mode {
 case "api": token, err = m.APIKey()
 case "chatgpt": token, err = m.AccessToken(ctx, "")
 default: return nil, errors.New("invalid provider authentication mode")
 }
 if err != nil { return nil, err }
 for attempt := 0; attempt < 2; attempt++ {
  req, e := http.NewRequestWithContext(ctx, method, m.apiURL+path, bytes.NewReader(body))
  if e != nil { return nil, e }
  req.Header.Set("Authorization", "Bearer "+token)
  req.Header.Set("Content-Type", "application/json")
  req.Header.Set("User-Agent", "amplio-native-openai/1")
  if path == "/responses" { req.Header.Set("Accept", "text/event-stream") }
  if mode == "api" {
   if v := os.Getenv("OPENAI_ORGANIZATION"); v != "" { req.Header.Set("OpenAI-Organization", v) }
   if v := os.Getenv("OPENAI_PROJECT"); v != "" { req.Header.Set("OpenAI-Project", v) }
  }
  // Stream lifetime belongs to the caller, not an OAuth body timeout.
  client := *m.client
  if path == "/responses" { client.Timeout = 0 }
  resp, e := client.Do(req)
  if e != nil {
   if ctx.Err() != nil { return nil, ctx.Err() }
   return nil, errors.New("OpenAI service is unreachable; credentials were kept")
  }
  if mode != "chatgpt" || resp.StatusCode != 401 || attempt != 0 { return resp, nil }
  remote := DecodeError(resp)
  _ = resp.Body.Close()
  if remote.Code == "subscription_sharing_invalid_user" { return nil, remote }
  token, e = m.AccessToken(ctx, token)
  if e != nil { return nil, e }
 }
 return nil, errors.New("OpenAI authorization failed")
}
func (m *Manager) Models(ctx context.Context, mode string) ([]Model, error) {
 ctx, cancel := context.WithTimeout(ctx, 30*time.Second); defer cancel()
 resp, err := m.Request(ctx, mode, "GET", "/models", nil)
 if err != nil { return nil, err }
 defer resp.Body.Close()
 if resp.StatusCode != 200 { return nil, DecodeError(resp) }
 var raw struct {
  Data []struct{ ID string `json:"id"` } `json:"data"`
  Models []struct{ Slug string `json:"slug"`; Name string `json:"display_name"`; Visibility string `json:"visibility"` } `json:"models"`
 }
 if err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&raw); err != nil { return nil, errors.New("invalid OpenAI model catalog") }
 models := []Model{}
 seen := map[string]bool{}
 if mode == "chatgpt" {
  if raw.Models == nil { return nil, errors.New("ChatGPT model catalog is unavailable for this account") }
  for _, x := range raw.Models {
   if x.Visibility == "list" && x.Slug != "" && !seen[x.Slug] {
    name := x.Name; if name == "" { name = x.Slug }
    models = append(models, Model{x.Slug, name}); seen[x.Slug] = true
   }
  }
 } else {
  if raw.Data == nil { return nil, errors.New("API model catalog is unavailable") }
  for _, x := range raw.Data {
   if x.ID != "" && !seen[x.ID] { models = append(models, Model{x.ID, x.ID}); seen[x.ID] = true }
  }
  sort.Slice(models, func(i,j int) bool { return strings.Compare(models[i].ID, models[j].ID) < 0 })
 }
 return models, nil
}
