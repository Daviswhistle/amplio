// SPDX-License-Identifier: Apache-2.0
package server
import (
 "bytes"
 "encoding/json"
 "errors"
 "io"
 "net"
 "net/http"
 "net/url"
 "regexp"
 "strings"
 "amplio/internal/llm"
 "amplio/internal/providerstate"
)
type providerHTTP struct{server *Server;accounts *providerstate.Manager;onReady func();app http.Handler}
// ProviderHandler adds setup without replacing the agent loop. Credential
// endpoints require the owner token, including status and model discovery.
func(s *Server)ProviderHandler(accounts *providerstate.Manager,onReady func())http.Handler{
 p:=&providerHTTP{server:s,accounts:accounts,onReady:onReady,app:s.Handler()};mux:=http.NewServeMux()
 handle:=func(pattern string,fn http.HandlerFunc){mux.HandleFunc(pattern,s.requireAuth(p.guard(fn)))}
 handle("GET /api/providers",p.status)
 handle("POST /api/providers/api-key",p.saveKey);handle("DELETE /api/providers/api-key",p.deleteKey)
 handle("POST /api/providers/chatgpt/login",p.startLogin);handle("DELETE /api/providers/chatgpt/login",p.cancelLogin)
 handle("POST /api/providers/chatgpt/callback",p.completeLogin);handle("DELETE /api/providers/chatgpt",p.disconnect)
 handle("GET /api/providers/models",p.catalog);handle("POST /api/providers/models",p.addModel);handle("PUT /api/providers/settings",p.settings)
 // Old URLs resolve to native implementations; no Python process is started.
 handle("GET /api/connections",p.status);handle("POST /api/connections/api-key",p.saveKey);handle("DELETE /api/connections/api-key",p.deleteKey)
 handle("POST /api/connections/chatgpt/start",p.startLogin);handle("DELETE /api/connections/chatgpt",p.disconnect)
 mux.HandleFunc("GET /api/models",p.menu);mux.HandleFunc("POST /api/runs",s.requireAuth(p.startRun));mux.Handle("/",p.app);return mux
}
func(p *providerHTTP)guard(next http.HandlerFunc)http.HandlerFunc{
 return func(w http.ResponseWriter,r *http.Request){
  w.Header().Set("Cache-Control","no-store")
  if p.server.token==""{writeErr(w,403,"provider setup requires an authenticated Amplio server");return}
  host,_,err:=net.SplitHostPort(r.RemoteAddr);ip:=net.ParseIP(host)
  if r.TLS==nil&&(err!=nil||ip==nil||!ip.IsLoopback()){writeErr(w,403,"use HTTPS or a loopback/SSH connection to manage providers");return}
  if origin:=r.Header.Get("Origin");origin!=""{
   u,err:=url.Parse(origin)
   if err!=nil||u.User!=nil||u.Path!=""||u.RawQuery!=""||!strings.EqualFold(u.Host,r.Host)||(u.Scheme!="http"&&u.Scheme!="https")||(r.TLS!=nil&&u.Scheme!="https"){writeErr(w,403,"cross-origin provider request denied");return}
  }
  if site:=r.Header.Get("Sec-Fetch-Site");site!=""&&site!="same-origin"&&site!="none"{writeErr(w,403,"cross-origin provider request denied");return}
  next(w,r)
 }
}
func decodeProvider(w http.ResponseWriter,r *http.Request,v any)error{
 r.Body=http.MaxBytesReader(w,r.Body,32<<10);dec:=json.NewDecoder(r.Body);dec.DisallowUnknownFields()
 if err:=dec.Decode(v);err!=nil{return errors.New("invalid provider request")};if err:=dec.Decode(new(any));err!=io.EOF{return errors.New("provider request must contain one JSON value")};return nil
}
func(p *providerHTTP)status(w http.ResponseWriter,r *http.Request){s,err:=p.accounts.Status();if err!=nil{writeErr(w,500,err.Error());return};writeJSON(w,200,s)}
func(p *providerHTTP)saveKey(w http.ResponseWriter,r *http.Request){
 var in struct{Key string `json:"key"`};if err:=decodeProvider(w,r,&in);err!=nil{writeErr(w,400,err.Error());return}
 if err:=p.accounts.SetAPIKey(in.Key);err!=nil{writeErr(w,400,err.Error());return};p.status(w,r)
}
func(p *providerHTTP)deleteKey(w http.ResponseWriter,r *http.Request){if err:=p.accounts.DeleteAPIKey();err!=nil{writeErr(w,409,err.Error());return};p.status(w,r)}
func(p *providerHTTP)startLogin(w http.ResponseWriter,r *http.Request){
 var in struct{NewAccount bool `json:"new_account"`}
 if r.ContentLength!=0{if err:=decodeProvider(w,r,&in);err!=nil{writeErr(w,400,err.Error());return}}
 start,err:=p.accounts.BeginLogin(p.server.runCtx,in.NewAccount);if err!=nil{writeErr(w,409,err.Error());return};writeJSON(w,200,start)
}
func(p *providerHTTP)cancelLogin(w http.ResponseWriter,r *http.Request){p.accounts.CancelLogin();p.status(w,r)}
func(p *providerHTTP)completeLogin(w http.ResponseWriter,r *http.Request){
 var in struct{URL string `json:"url"`};if err:=decodeProvider(w,r,&in);err!=nil{writeErr(w,400,err.Error());return}
 if _,err:=p.accounts.CompleteLogin(in.URL);err!=nil{writeErr(w,400,err.Error());return};p.status(w,r)
}
func(p *providerHTTP)disconnect(w http.ResponseWriter,r *http.Request){if err:=p.accounts.Disconnect();err!=nil{writeErr(w,500,err.Error());return};p.status(w,r)}
func(p *providerHTTP)catalog(w http.ResponseWriter,r *http.Request){
 mode:=r.URL.Query().Get("provider");if mode=="openai"{mode="api"};if mode!="api"&&mode!="chatgpt"{writeErr(w,400,"choose ChatGPT or OpenAI API");return}
 models,err:=p.accounts.Models(r.Context(),mode);if err!=nil{writeErr(w,502,err.Error());return};writeJSON(w,200,map[string]any{"models":models})
}
type providerChoice struct{Provider string `json:"provider"`;Model string `json:"model"`;Effort string `json:"effort"`}
var modelID=regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._/-]{0,199}$`)
func(c providerChoice)spec()(string,error){
 prefix:="";switch c.Provider{case "chatgpt":prefix="chatgpt";case "openai":prefix="openai-responses";default:return "",errors.New("choose ChatGPT or OpenAI API")}
 model:=strings.TrimSpace(c.Model);if !modelID.MatchString(model){return "",errors.New("choose a valid model ID")}
 switch c.Effort{case "","none","minimal","low","medium","high","xhigh","max":default:return "",errors.New("invalid reasoning level")}
 spec:=prefix+":"+model;if c.Effort!=""{spec+="?reasoning.effort="+url.QueryEscape(c.Effort)};return spec,nil
}
func(p *providerHTTP)choice(c providerChoice)(string,error){
 spec,err:=c.spec();if err!=nil{return "",err};status,err:=p.accounts.Status();if err!=nil{return "",err}
 if c.Provider=="chatgpt"&&!status.ChatGPTConnected{return "",errors.New("connect ChatGPT and grant plan access first")}
 if c.Provider=="openai"&&status.APIKeySource=="none"{return "",errors.New("save an OpenAI API key first")};return spec,nil
}
func(p *providerHTTP)addModel(w http.ResponseWriter,r *http.Request){
 var in providerChoice;if err:=decodeProvider(w,r,&in);err!=nil{writeErr(w,400,err.Error());return}
 spec,err:=p.choice(in);if err!=nil{writeErr(w,400,err.Error());return}
 if err=p.server.store.AddCustomModel(r.Context(),spec);err!=nil{writeErr(w,500,"could not save model");return};writeJSON(w,200,map[string]string{"spec":spec})
}
func(p *providerHTTP)settings(w http.ResponseWriter,r *http.Request){
 var in struct{Agent providerChoice `json:"agent"`;HQ providerChoice `json:"hq"`;Fast providerChoice `json:"fast"`}
 if err:=decodeProvider(w,r,&in);err!=nil{writeErr(w,400,err.Error());return}
 a,err:=p.choice(in.Agent);if err!=nil{writeErr(w,400,err.Error());return}
 h,err:=p.choice(in.HQ);if err!=nil{writeErr(w,400,err.Error());return}
 f,err:=p.choice(in.Fast);if err!=nil{writeErr(w,400,err.Error());return}
 // Menu additions cannot change billing defaults. Commit all three defaults
 // together only after validation and menu persistence succeed.
 for _,spec:=range []string{a,h,f}{if err=p.server.store.AddCustomModel(r.Context(),spec);err!=nil{writeErr(w,500,"could not update model menu");return}}
 if err=p.accounts.SaveSettings(providerstate.Settings{Agent:a,HQ:h,Fast:f});err!=nil{writeErr(w,500,err.Error());return}
 if p.onReady!=nil{p.onReady()};p.status(w,r)
}
func(p *providerHTTP)menu(w http.ResponseWriter,r *http.Request){
 settings,err:=p.accounts.Settings();if err!=nil{writeErr(w,500,"provider settings could not be read");return}
 custom,err:=p.server.store.ListCustomModels(r.Context());if err!=nil{writeErr(w,500,err.Error());return}
 menu:=modelMenu{Default:settings.Agent,Models:[]modelEntry{}};seen:=map[string]bool{}
 add:=func(spec string,removable bool){if spec==""||seen[spec]||strings.HasPrefix(spec,"managed:"){return};seen[spec]=true;menu.Models=append(menu.Models,modelEntry{Spec:spec,Label:llm.ShortLabel(spec),Removable:removable})}
 for _,spec:=range []string{settings.Agent,settings.HQ,settings.Fast}{add(spec,false)}
 for _,spec:=range p.server.defaults.LLMs{add(spec,false)};for _,spec:=range custom{add(spec,true)}
 markDuplicates(menu.Models);writeJSON(w,200,menu)
}
func(p *providerHTTP)startRun(w http.ResponseWriter,r *http.Request){
 settings,err:=p.accounts.Settings();if err!=nil{writeErr(w,500,"provider settings could not be read");return}
 if !settings.Ready(){writeErr(w,409,"finish setup in Providers before starting a run");return}
 r.Body=http.MaxBytesReader(w,r.Body,16<<20);var body map[string]json.RawMessage;dec:=json.NewDecoder(r.Body)
 if dec.Decode(&body)!=nil||body==nil{writeErr(w,400,"invalid JSON body");return};if err:=dec.Decode(new(any));err!=io.EOF{writeErr(w,400,"expected one JSON request");return}
 var spec string;if raw,ok:=body["llm"];ok{if json.Unmarshal(raw,&spec)!=nil{writeErr(w,400,"llm must be a string");return}}
 if spec==""{spec=settings.Agent};if strings.HasPrefix(spec,"managed:"){writeErr(w,400,"choose a concrete agent model; managed bindings are for system tasks only");return}
 body["llm"],_=json.Marshal(spec);b,_:=json.Marshal(body);r.Body=io.NopCloser(bytes.NewReader(b));r.ContentLength=int64(len(b));p.app.ServeHTTP(w,r)
}
