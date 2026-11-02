// SPDX-License-Identifier: Apache-2.0
package providerstate

import (
 "bytes"
 "context"
 "crypto"
 "crypto/rand"
 "crypto/rsa"
 "crypto/sha256"
 "crypto/subtle"
 "encoding/base64"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "math/big"
 "net"
 "net/http"
 "net/url"
 "os"
 "path/filepath"
 "strconv"
 "strings"
 "time"
)
const resource="https://api.openai.com/v1"
const issuer="https://auth.openai.com"
const scopes="openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"

type attempt struct {
 state,nonce,verifier,redirect,clientID,subject string
 epoch uint64
 expires time.Time
 listener net.Listener
 cancel context.CancelFunc
 ctx context.Context
 completing bool
}
type LoginStart struct {
 AuthorizationURL string `json:"authorization_url"`
 RedirectURI string `json:"redirect_uri"`
 ExpiresAt time.Time `json:"expires_at"`
}
func randomValue(n int)(string,error){b:=make([]byte,n);if _,err:=rand.Read(b);err!=nil{return "",err};return base64.RawURLEncoding.EncodeToString(b),nil}
func (m *Manager) hostIDLocked()(string,error){
 p:=filepath.Join(m.credentials(),"host_id");b,err:=readPrivate(p,128)
 if err==nil{v:=strings.TrimSpace(string(b));if v==""{return "",errors.New("empty OAuth host ID")};return v,nil}
 if !errors.Is(err,os.ErrNotExist){return "",err}
 id,err:=randomValue(24);if err!=nil{return "",err};id="amplio:"+id;return id,writePrivate(p,[]byte(id+"\n"),true)
}
// Start the callback listener before returning the authorization URL. The UI
// opens the browser; no external CLI, Python interpreter or helper is needed.
func (m *Manager) BeginLogin(parent context.Context,newAccount bool)(LoginStart,error){
 m.loginMu.Lock();defer m.loginMu.Unlock()
 if m.pending!=nil{return LoginStart{},errors.New("sign-in is already in progress; cancel it before starting another")}
 m.mu.Lock();old,err:=m.accountLocked();if newAccount{old=account{}};host,hostErr:=m.hostIDLocked();epoch:=m.epoch;m.mu.Unlock()
 if err!=nil{return LoginStart{},err};if hostErr!=nil{return LoginStart{},hostErr}
 state,err:=randomValue(32);if err!=nil{return LoginStart{},err}
 nonce,err:=randomValue(32);if err!=nil{return LoginStart{},err}
 verifier,err:=randomValue(48);if err!=nil{return LoginStart{},err}
 ln,err:=net.Listen("tcp4","127.0.0.1:0");if err!=nil{return LoginStart{},errors.New("cannot open the local sign-in callback")}
 ctx,cancel:=context.WithTimeout(parent,5*time.Minute)
 a:=&attempt{state:state,nonce:nonce,verifier:verifier,clientID:old.ClientID,subject:old.Subject,epoch:epoch,expires:m.now().Add(5*time.Minute),listener:ln,cancel:cancel,ctx:ctx,redirect:"http://"+ln.Addr().String()+"/auth/callback"}
 sum:=sha256.Sum256([]byte(verifier))
 q:=url.Values{"client_id":{old.ClientID},"response_type":{"code"},"redirect_uri":{a.redirect},"scope":{scopes},"resource":{resource},"state":{state},"nonce":{nonce},"code_challenge_method":{"S256"},"code_challenge":{base64.RawURLEncoding.EncodeToString(sum[:])},"ext_agent_host_id":{host}}
 if old.ClientID==""{q.Set("client_id","dynamic_agent_client");q.Set("agent_name_hint","Amplio")}else{if old.IDToken!=""{q.Set("id_token_hint",old.IDToken)};if !old.permitted(){q.Set("prompt","consent")}}
 m.pending=a;m.loginState="pending";m.loginError=""
 srv:=&http.Server{ReadHeaderTimeout:5*time.Second,ReadTimeout:10*time.Second,WriteTimeout:45*time.Second}
 srv.Handler=http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  w.Header().Set("Cache-Control","no-store");w.Header().Set("Referrer-Policy","no-referrer");w.Header().Set("Content-Security-Policy","default-src 'none'; frame-ancestors 'none'")
  if r.Method!="GET"||r.URL.Path!="/auth/callback"{http.NotFound(w,r);return}
  // Use the registered origin, never the request's Host or forwarded headers.
  _,err:=m.CompleteLogin(a.redirect+"?"+r.URL.RawQuery)
  w.Header().Set("Content-Type","text/plain; charset=utf-8")
  if err!=nil{w.WriteHeader(http.StatusBadRequest);_,_=io.WriteString(w,"Sign-in could not be completed. Return to Amplio Providers for details.")}else{_,_=io.WriteString(w,"Sign-in completed. Return to Amplio Providers; this tab can be closed.")}
 })
 go func(){_=srv.Serve(ln)}()
 go func(){<-ctx.Done();_=ln.Close();m.loginMu.Lock();defer m.loginMu.Unlock();if m.pending==a{m.pending=nil;m.loginState="error";m.loginError="Sign-in expired or was cancelled. Start again."}}()
 return LoginStart{m.authorizeURL+"?"+q.Encode(),a.redirect,a.expires},nil
}
// CompleteLogin also accepts a full callback URL from the authenticated remote
// UI. It requires the exact pending redirect and state; codes remain one-use.
func (m *Manager) CompleteLogin(returned string)(bool,error){
 if len(returned)>16384{return false,errors.New("authorization return URL is too long")}
 u,err:=url.Parse(strings.TrimSpace(returned));if err!=nil{return false,errors.New("invalid authorization return URL")}
 q,err:=url.ParseQuery(u.RawQuery);if err!=nil{return false,errors.New("invalid callback query")}
 m.loginMu.Lock();a:=m.pending
 if a==nil||!m.now().Before(a.expires){m.loginMu.Unlock();return false,errors.New("no pending sign-in; start again")}
 if a.completing{m.loginMu.Unlock();return false,errors.New("sign-in is already being completed")}
 base:=*u;base.RawQuery="";base.Fragment=""
 if u.User!=nil||u.Fragment!=""||base.String()!=a.redirect||len(q["state"])!=1||subtle.ConstantTimeCompare([]byte(q.Get("state")),[]byte(a.state))!=1{m.loginMu.Unlock();return false,errors.New("authorization return URL or state does not match this sign-in")}
 for _,key:=range []string{"code","client_id","error"}{if len(q[key])>1{m.loginMu.Unlock();return false,errors.New("duplicate OAuth callback parameter")}}
 a.completing=true;m.loginState="completing";m.loginMu.Unlock()
 var completionErr error
 defer func(){
  m.loginMu.Lock();if m.pending==a{m.pending=nil;if completionErr!=nil{m.loginState="error";m.loginError=completionErr.Error()}else{m.loginState="complete";m.loginError=""}};m.loginMu.Unlock();a.cancel();_=a.listener.Close()
 }()
 fail:=func(e error)(bool,error){completionErr=e;return false,e}
 if q.Get("error")!=""{return fail(errors.New("ChatGPT authorization was declined; existing credentials were kept"))}
 code:=q.Get("code");if code==""{return fail(errors.New("authorization callback has no code"))}
 clientID:=a.clientID
 if clientID==""{clientID=q.Get("client_id");if !strings.HasPrefix(clientID,"oaiapp_")||len(clientID)>256{return fail(errors.New("registration did not issue a valid client ID"))}}else if q.Get("client_id")!=""&&q.Get("client_id")!=clientID{return fail(errors.New("returning OAuth client ID mismatch"))}
 payload,err:=m.token(a.ctx,url.Values{"grant_type":{"authorization_code"},"client_id":{clientID},"code":{code},"code_verifier":{a.verifier},"redirect_uri":{a.redirect},"resource":{resource}})
 if err!=nil{return fail(err)}
 claims,err:=m.verifyIDToken(a.ctx,payload.IDToken,clientID,a.nonce);if err!=nil{return fail(err)}
 if a.subject!=""&&claims.Subject!=a.subject{return fail(errors.New("ChatGPT account changed; use Switch account explicitly"))}
 saved,err:=m.record(payload,clientID,claims.Subject,claims.Email);if err!=nil{return fail(err)}
 m.mu.Lock();defer m.mu.Unlock()
 if a.ctx.Err()!=nil||m.epoch!=a.epoch{return fail(errors.New("sign-in was cancelled; credentials were not changed"))}
 if err=writeJSONPrivate(m.accountPath(),saved,true);err!=nil{return fail(err)}
 return saved.permitted(),nil
}
func (m *Manager) CancelLogin(){m.loginMu.Lock();a:=m.pending;m.pending=nil;m.loginState="idle";m.loginError="";m.loginMu.Unlock();if a!=nil{a.cancel();_=a.listener.Close()}}
func (m *Manager) Close(){m.CancelLogin()}

type tokenPayload struct {
 AccessToken string `json:"access_token"`
 RefreshToken string `json:"refresh_token"`
 IDToken string `json:"id_token"`
 TokenType string `json:"token_type"`
 Scope *string `json:"scope"`
 ExpiresIn int64 `json:"expires_in"`
}
type RemoteError struct {Status int;Code string;RequestID string}
func (e *RemoteError) Error()string{
 s:=fmt.Sprintf("OpenAI request failed: HTTP %d (%s)",e.Status,e.Code)
 if e.Code=="context_length_exceeded"||e.Code=="context_window_exceeded"{s+="; maximum context length exceeded"}
 if e.RequestID!=""{s+="; request ID "+e.RequestID};return s
}
func safeCode(s string)string{
 if s==""||len(s)>120{return "request_failed"}
 for _,r:=range s{if !(r>='a'&&r<='z'||r>='A'&&r<='Z'||r>='0'&&r<='9'||r=='_'||r=='-'){return "request_failed"}};return s
}
func DecodeError(resp *http.Response)*RemoteError{
 var p struct{Error json.RawMessage};b,_:=io.ReadAll(io.LimitReader(resp.Body,8192));_=json.Unmarshal(b,&p)
 var code string
 if json.Unmarshal(p.Error,&code)!=nil{var v struct{Code,Type string};_=json.Unmarshal(p.Error,&v);code=v.Code;if code==""{code=v.Type}}
 id:=resp.Header.Get("x-request-id");if id!=""&&safeCode(id)=="request_failed"{id=""}
 return &RemoteError{resp.StatusCode,safeCode(code),id}
}
func (m *Manager) token(ctx context.Context,values url.Values)(tokenPayload,error){
 var result tokenPayload
 req,err:=http.NewRequestWithContext(ctx,"POST",m.tokenURL,strings.NewReader(values.Encode()));if err!=nil{return result,err}
 req.Header.Set("Content-Type","application/x-www-form-urlencoded")
 resp,err:=m.client.Do(req);if err!=nil{return result,errors.New("OAuth service is unavailable; credentials were kept")};defer resp.Body.Close()
 if resp.StatusCode!=200{return result,DecodeError(resp)}
 if err=json.NewDecoder(io.LimitReader(resp.Body,128<<10)).Decode(&result);err!=nil{return result,errors.New("invalid OAuth response")}
 if result.AccessToken==""||result.ExpiresIn<=0||result.ExpiresIn>31*24*3600||(result.TokenType!=""&&!strings.EqualFold(result.TokenType,"Bearer")){return result,errors.New("invalid OAuth credentials or expiry")}
 return result,nil
}
func (m *Manager) record(p tokenPayload,client,subject,email string)(account,error){
 var granted []string;if p.Scope!=nil{granted=strings.Fields(*p.Scope)}
 a:=account{Subject:subject,Email:email,ClientID:client,IDToken:p.IDToken,AccessToken:p.AccessToken,RefreshToken:p.RefreshToken,Scopes:granted,ExpiresAt:m.now().Add(time.Duration(p.ExpiresIn)*time.Second)}
 if a.permitted()&&a.RefreshToken==""{return account{},errors.New("ChatGPT plan access requires a renewable grant")};return a,nil
}
func terminalRefresh(code string)bool{
 switch code{case "invalid_grant","invalid_refresh_token","token_expired","refresh_token_expired","refresh_token_invalidated","refresh_token_reused":return true};return false
}
// Serialize token refresh. If another call already replaced a rejected token,
// reuse the replacement instead of racing a rotating refresh credential.
func (m *Manager) AccessToken(ctx context.Context,rejected string)(string,error){
 m.mu.Lock();defer m.mu.Unlock();a,err:=m.accountLocked();if err!=nil{return "",err}
 if !a.permitted()||a.RefreshToken==""{return "",errors.New("sign in with ChatGPT and grant plan access in Providers")}
 if a.AccessToken!=""&&m.now().Add(time.Minute).Before(a.ExpiresAt)&&(rejected==""||rejected!=a.AccessToken){return a.AccessToken,nil}
 p,err:=m.token(ctx,url.Values{"grant_type":{"refresh_token"},"client_id":{a.ClientID},"refresh_token":{a.RefreshToken},"resource":{resource}})
 if err!=nil{
  var remote *RemoteError
  if errors.As(err,&remote)&&terminalRefresh(remote.Code){
   a.AccessToken="";a.RefreshToken="";a.ExpiresAt=time.Time{}
   if e:=writeJSONPrivate(m.accountPath(),a,true);e!=nil{return "",e}
   return "",errors.New("ChatGPT authorization expired or was revoked; sign in again in Providers")
  }
  // A proactive refresh outage does not invalidate a still-valid token. Never
  // reuse a token explicitly rejected by the resource server with HTTP 401.
  if a.AccessToken!=""&&m.now().Before(a.ExpiresAt)&&(rejected==""||rejected!=a.AccessToken){return a.AccessToken,nil}
  return "",err
 }
 if p.RefreshToken==""{p.RefreshToken=a.RefreshToken}
 if p.Scope==nil{old:=strings.Join(a.Scopes," ");p.Scope=&old}
 if p.IDToken!=""{
  c,e:=m.verifyIDToken(ctx,p.IDToken,a.ClientID,"");if e!=nil{return "",e};if c.Subject!=a.Subject{return "",errors.New("refreshed ChatGPT identity mismatch")}
 }else{p.IDToken=a.IDToken}
 next,err:=m.record(p,a.ClientID,a.Subject,a.Email);if err!=nil{return "",err}
 if err=writeJSONPrivate(m.accountPath(),next,true);err!=nil{return "",err}
 if !next.permitted(){return "",errors.New("ChatGPT plan permission was removed; authorize it again in Providers")}
 return next.AccessToken,nil
}
type identity struct{Subject,Email string}
// Accept only a published RSA key and validate the OIDC claims. Token-controlled
// key URLs are never followed, and no runtime JWT package is downloaded.
func (m *Manager) verifyIDToken(ctx context.Context,token,audience,nonce string)(identity,error){
 invalid:=errors.New("ChatGPT identity token verification failed")
 if token==""||len(token)>32768{return identity{},invalid}
 parts:=strings.Split(token,".");if len(parts)!=3{return identity{},invalid}
 h,err:=base64.RawURLEncoding.DecodeString(parts[0]);if err!=nil{return identity{},invalid}
 var header struct{Alg,Kid string}
 if json.Unmarshal(h,&header)!=nil||header.Alg!="RS256"||header.Kid==""{return identity{},invalid}
 req,err:=http.NewRequestWithContext(ctx,"GET",m.jwksURL,nil);if err!=nil{return identity{},invalid}
 resp,err:=m.client.Do(req);if err!=nil{return identity{},errors.New("cannot retrieve OpenAI signing keys")};defer resp.Body.Close()
 if resp.StatusCode!=200{return identity{},errors.New("OpenAI signing keys unavailable")}
 var jwks struct{Keys []struct{Kty,Kid,Alg,Use,N,E string}}
 if json.NewDecoder(io.LimitReader(resp.Body,256<<10)).Decode(&jwks)!=nil{return identity{},invalid}
 var key *rsa.PublicKey
 for _,j:=range jwks.Keys{
  if j.Kid!=header.Kid||j.Kty!="RSA"||(j.Alg!=""&&j.Alg!="RS256")||(j.Use!=""&&j.Use!="sig"){continue}
  n,e1:=base64.RawURLEncoding.DecodeString(j.N);e,e2:=base64.RawURLEncoding.DecodeString(j.E)
  if e1!=nil||e2!=nil||len(e)>4||len(e)==0{return identity{},invalid}
  exponent:=0;for _,b:=range e{exponent=exponent*256+int(b)}
  candidate:=&rsa.PublicKey{N:new(big.Int).SetBytes(n),E:exponent}
  if candidate.N.BitLen()<2048||exponent<3||exponent%2==0{return identity{},invalid};key=candidate;break
 }
 if key==nil{return identity{},invalid}
 sig,err:=base64.RawURLEncoding.DecodeString(parts[2]);if err!=nil{return identity{},invalid}
 sum:=sha256.Sum256([]byte(parts[0]+"."+parts[1]));if rsa.VerifyPKCS1v15(key,crypto.SHA256,sum[:],sig)!=nil{return identity{},invalid}
 body,err:=base64.RawURLEncoding.DecodeString(parts[1]);if err!=nil{return identity{},invalid}
 var claims map[string]any;dec:=json.NewDecoder(bytes.NewReader(body));dec.UseNumber();if dec.Decode(&claims)!=nil{return identity{},invalid}
 sub,_:=claims["sub"].(string);email,_:=claims["email"].(string)
 if claims["iss"]!=issuer||sub==""{return identity{},invalid}
 audOK:=false
 switch a:=claims["aud"].(type){case string:audOK=a==audience;case []any:for _,v:=range a{if v==audience{audOK=true}};if len(a)>1&&claims["azp"]!=audience{audOK=false}}
 if !audOK{return identity{},invalid}
 seconds:=func(v any)(int64,bool){n,ok:=v.(json.Number);if !ok{return 0,false};x,e:=strconv.ParseInt(string(n),10,64);return x,e==nil}
 exp,ok:=seconds(claims["exp"]);if !ok||exp<=m.now().Unix()-60{return identity{},invalid}
 if raw,exists:=claims["nbf"];exists{n,ok:=seconds(raw);if !ok||n>m.now().Unix()+60{return identity{},invalid}}
 if nonce!=""{actual,_:=claims["nonce"].(string);if subtle.ConstantTimeCompare([]byte(actual),[]byte(nonce))!=1{return identity{},invalid}}
 return identity{sub,email},nil
}
