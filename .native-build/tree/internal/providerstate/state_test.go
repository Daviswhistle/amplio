// SPDX-License-Identifier: Apache-2.0
package providerstate
import (
 "context"
 "crypto"
 "crypto/rand"
 "crypto/rsa"
 "crypto/sha256"
 "encoding/base64"
 "encoding/json"
 "errors"
 "math/big"
 "net/http"
 "net/http/httptest"
 "net/url"
 "os"
 "path/filepath"
 "strings"
 "sync"
 "sync/atomic"
 "testing"
 "time"
)
var testRSA struct{sync.Once;key *rsa.PrivateKey}
func signingKey(t *testing.T)*rsa.PrivateKey{t.Helper();testRSA.Do(func(){var err error;testRSA.key,err=rsa.GenerateKey(rand.Reader,2048);if err!=nil{panic(err)}});return testRSA.key}
func jwtFor(t *testing.T,claims map[string]any)string{
 t.Helper();h:=base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`));b,_:=json.Marshal(claims);body:=h+"."+base64.RawURLEncoding.EncodeToString(b);sum:=sha256.Sum256([]byte(body));sig,err:=rsa.SignPKCS1v15(rand.Reader,signingKey(t),crypto.SHA256,sum[:]);if err!=nil{t.Fatal(err)};return body+"."+base64.RawURLEncoding.EncodeToString(sig)
}
func fixture(t *testing.T)(*Manager,*http.ServeMux){
 t.Helper();t.Setenv("OPENAI_API_KEY","");t.Setenv("AMPLIO_OPENAI_KEY_FILE","")
 mux:=http.NewServeMux();key:=signingKey(t)
 mux.HandleFunc("/jwks",func(w http.ResponseWriter,r *http.Request){_=json.NewEncoder(w).Encode(map[string]any{"keys":[]any{map[string]any{"kty":"RSA","kid":"test","alg":"RS256","use":"sig","n":base64.RawURLEncoding.EncodeToString(key.N.Bytes()),"e":base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})})
 ts:=httptest.NewServer(mux);t.Cleanup(ts.Close);m:=newManager(t.TempDir());m.jwksURL=ts.URL+"/jwks";m.tokenURL=ts.URL+"/token";m.authorizeURL=ts.URL+"/authorize";m.apiURL=ts.URL+"/v1";t.Cleanup(m.Close);return m,mux
}
func claimsFor(m *Manager,nonce string)map[string]any{return map[string]any{"iss":issuer,"aud":"oaiapp_test","sub":"account-a","email":"a@example.test","exp":m.now().Add(time.Hour).Unix(),"nonce":nonce}}
func saveAccount(t *testing.T,m *Manager,a account){t.Helper();if err:=writeJSONPrivate(m.accountPath(),a,true);err!=nil{t.Fatal(err)}}
func granted()[]string{return strings.Fields(scopes)}
func authorize(t *testing.T,m *Manager,mux *http.ServeMux,scope string)(LoginStart,string){
 t.Helper();start,err:=m.BeginLogin(context.Background(),false);if err!=nil{t.Fatal(err)};u,_:=url.Parse(start.AuthorizationURL);q:=u.Query()
 mux.HandleFunc("/token",func(w http.ResponseWriter,r *http.Request){_=r.ParseForm();if r.Form.Get("client_id")!="oaiapp_test"||r.Form.Get("redirect_uri")!=start.RedirectURI||r.Form.Get("resource")!=resource||r.Form.Get("code_verifier")==""{t.Error("OAuth exchange contract lost")};_=json.NewEncoder(w).Encode(map[string]any{"access_token":"oauth-secret","refresh_token":"refresh-secret","id_token":jwtFor(t,claimsFor(m,q.Get("nonce"))),"scope":scope,"expires_in":3600,"token_type":"Bearer"})})
 return start,start.RedirectURI+"?"+url.Values{"state":{q.Get("state")},"code":{"one-use-code"},"client_id":{"oaiapp_test"}}.Encode()
}
func TestSettingsPersistWithoutConfigOrCredentials(t *testing.T){
 m,_:=fixture(t);s,err:=m.Settings();if err!=nil||s.Ready(){t.Fatalf("unexpected initial state %+v %v",s,err)}
 if _,err=m.Selected("agent");!errors.Is(err,ErrNotConfigured){t.Fatal(err)}
 settings:=Settings{"chatgpt:gpt-test","chatgpt:gpt-test","openai-responses:gpt-small"};if err=m.SaveSettings(settings);err!=nil{t.Fatal(err)}
 loaded,err:=newManager(m.dir).Settings();if err!=nil||loaded!=settings{t.Fatalf("settings not durable: %+v %v",loaded,err)}
 if _,err=os.Stat(filepath.Join(m.dir,"config.toml"));!os.IsNotExist(err){t.Fatal("config.toml should not be generated or overwritten")}
}
func TestSettingsCorruptionDoesNotFallBackBilling(t *testing.T){m,_:=fixture(t);m.SetFallback(Settings{"openai:paid","openai:paid","openai:paid"});_=os.WriteFile(filepath.Join(m.dir,"providers.json"),[]byte("bad-json"),0600);if _,err:=m.Selected("agent");err==nil{t.Fatal("corruption silently selected another billing source")}}
func TestPrivateAPIKeyAndEnvironmentPrecedence(t *testing.T){
 m,_:=fixture(t);if err:=m.SetAPIKey("sk-stored-secret");err!=nil{t.Fatal(err)};info,_:=os.Stat(m.keyPath());if info.Mode().Perm()!=0600{t.Fatal(info.Mode())};dir,_:=os.Stat(filepath.Dir(m.keyPath()));if dir.Mode().Perm()!=0700{t.Fatal(dir.Mode())}
 status,err:=m.Status();if err!=nil{t.Fatal(err)};b,_:=json.Marshal(status);if strings.Contains(string(b),"secret"){t.Fatal("status disclosed secret")}
 t.Setenv("OPENAI_API_KEY","sk-environment-secret");key,_:=m.APIKey();if key!="sk-environment-secret"{t.Fatal(key)}
 if m.SetAPIKey("sk-new-secret")==nil||m.DeleteAPIKey()==nil{t.Fatal("environment-owned key changed")}
 t.Setenv("OPENAI_API_KEY","");if err=m.DeleteAPIKey();err!=nil{t.Fatal(err)};if _,err=m.APIKey();err==nil{t.Fatal("deleted key still usable")}
}
func TestCredentialFilesRejectSymlinksAndPublicPermissions(t *testing.T){
 m,_:=fixture(t);_=os.MkdirAll(m.credentials(),0700);target:=filepath.Join(t.TempDir(),"outside");_=os.WriteFile(target,[]byte("sk-outside"),0600);_=os.Symlink(target,m.keyPath())
 if _,err:=m.APIKey();err==nil{t.Fatal("followed symlink")};if m.SetAPIKey("sk-newsecret")==nil{t.Fatal("overwrote symlink")};_=os.Remove(m.keyPath());_=os.WriteFile(m.keyPath(),[]byte("sk-public"),0644);if _,err:=m.APIKey();err==nil{t.Fatal("accepted public key")}
}
func TestOIDCClaimAndSignatureValidation(t *testing.T){
 m,_:=fixture(t)
 for _,name:=range []string{"valid","issuer","audience","expiry","nonce","subject","signature","algorithm","azp"}{t.Run(name,func(t *testing.T){
  c:=claimsFor(m,"nonce")
  switch name{case "issuer":c["iss"]="https://evil.test";case "audience":c["aud"]="other";case "expiry":c["exp"]=m.now().Add(-time.Hour).Unix();case "nonce":c["nonce"]="wrong";case "subject":delete(c,"sub");case "azp":c["aud"]=[]string{"oaiapp_test","other"}}
  token:=jwtFor(t,c)
  if name=="signature"{p:=strings.Split(token,".");p[2]=base64.RawURLEncoding.EncodeToString(make([]byte,256));token=strings.Join(p,".")}
  if name=="algorithm"{p:=strings.Split(token,".");p[0]=base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"test"}`));token=strings.Join(p,".")}
  _,err:=m.verifyIDToken(context.Background(),token,"oaiapp_test","nonce");if (err==nil)!=(name=="valid"){t.Fatalf("validation %s: %v",name,err)}
 })}
}
func TestBrowserCallbackUsesNativeOAuth(t *testing.T){
 m,mux:=fixture(t);start,callback:=authorize(t,m,mux,scopes);u,_:=url.Parse(start.AuthorizationURL)
 if u.Query().Get("client_id")!="dynamic_agent_client"||u.Query().Get("code_challenge_method")!="S256"{t.Fatal("missing PKCE/registration")}
 resp,err:=http.Get(callback);if err!=nil{t.Fatal(err)};_=resp.Body.Close();if resp.StatusCode!=200{t.Fatal(resp.StatusCode)}
 s,err:=m.Status();if err!=nil||!s.ChatGPTConnected||s.LoginPending{t.Fatalf("not connected: %+v %v",s,err)};if _,err=os.Stat(m.accountPath());err!=nil{t.Fatal(err)}
}
func TestRemotePasteSameRedirectAndSingleUse(t *testing.T){
 m,mux:=fixture(t);start,callback:=authorize(t,m,mux,scopes)
 if _,err:=m.CompleteLogin(strings.Replace(callback,"127.0.0.1","localhost",1));err==nil{t.Fatal("accepted substituted redirect host")}
 u,_:=url.Parse(callback);q:=u.Query();q.Set("state","wrong");u.RawQuery=q.Encode();if _,err:=m.CompleteLogin(u.String());err==nil{t.Fatal("accepted mismatched state")}
 s,_:=m.Status();if !s.LoginPending{t.Fatal("foreign callback consumed pending login")}
 if ok,err:=m.CompleteLogin(callback);err!=nil||!ok{t.Fatalf("remote completion: %v %v",ok,err)}
 if _,err:=m.CompleteLogin(callback);err==nil{t.Fatal("accepted callback replay")};if !strings.HasPrefix(start.RedirectURI,"http://127.0.0.1:"){t.Fatal(start.RedirectURI)}
}
func TestPermissionDeclineRetainsIdentityAndRequiresReconsent(t *testing.T){
 m,mux:=fixture(t);_,callback:=authorize(t,m,mux,"openid profile email offline_access");ok,err:=m.CompleteLogin(callback);if err!=nil||ok{t.Fatalf("identity-only result %v %v",ok,err)}
 s,_:=m.Status();if !s.ChatGPTSignedIn||s.ChatGPTConnected{t.Fatalf("wrong permission state %+v",s)};if _,err=m.AccessToken(context.Background(),"");err==nil{t.Fatal("identity-only token used for inference")}
 next,err:=m.BeginLogin(context.Background(),false);if err!=nil{t.Fatal(err)};u,_:=url.Parse(next.AuthorizationURL);if u.Query().Get("prompt")!="consent"||u.Query().Get("client_id")!="oaiapp_test"{t.Fatal("did not retain registration/reconsent")}
}
func TestCancelAndDeclineKeepExistingAccount(t *testing.T){
 m,_:=fixture(t);a:=account{Subject:"original",ClientID:"oaiapp_old",AccessToken:"saved-secret",RefreshToken:"renew",Scopes:granted(),ExpiresAt:time.Now().Add(time.Hour)};saveAccount(t,m,a)
 first,err:=m.BeginLogin(context.Background(),false);if err!=nil{t.Fatal(err)};m.CancelLogin();if _,err=m.CompleteLogin(first.RedirectURI+"?state=x&code=y");err==nil{t.Fatal("cancelled attempt accepted")}
 next,err:=m.BeginLogin(context.Background(),false);if err!=nil{t.Fatal(err)};u,_:=url.Parse(next.AuthorizationURL)
 _,err=m.CompleteLogin(next.RedirectURI+"?"+url.Values{"state":{u.Query().Get("state")},"error":{"access_denied"}}.Encode());if err==nil{t.Fatal("denial was success")}
 token,err:=m.AccessToken(context.Background(),"");if err!=nil||token!="saved-secret"{t.Fatal("denial lost old authorization")}
}
func TestConcurrentRefreshRotatesOnce(t *testing.T){
 m,mux:=fixture(t);var calls atomic.Int32
 saveAccount(t,m,account{Subject:"account-a",ClientID:"oaiapp_test",AccessToken:"expired",RefreshToken:"refresh-1",Scopes:granted(),ExpiresAt:time.Now().Add(-time.Hour)})
 mux.HandleFunc("/token",func(w http.ResponseWriter,r *http.Request){calls.Add(1);_=r.ParseForm();if r.Form.Get("refresh_token")!="refresh-1"||r.Form.Get("resource")!=resource{t.Error("refresh contract")};_=json.NewEncoder(w).Encode(map[string]any{"access_token":"fresh","refresh_token":"refresh-2","expires_in":3600,"scope":scopes})})
 var wg sync.WaitGroup
 for i:=0;i<8;i++{wg.Add(1);go func(){defer wg.Done();token,err:=m.AccessToken(context.Background(),"");if err!=nil||token!="fresh"{t.Errorf("refresh %s %v",token,err)}}()};wg.Wait()
 if calls.Load()!=1{t.Fatalf("refresh was performed %d times",calls.Load())};a,err:=m.accountLocked();if err!=nil||a.RefreshToken!="refresh-2"{t.Fatal("rotated token not saved")}
}
func TestRefreshRevocationVsTemporaryFailure(t *testing.T){
 for _,code:=range []string{"invalid_grant","server_error"}{t.Run(code,func(t *testing.T){
  m,mux:=fixture(t);saveAccount(t,m,account{Subject:"account-a",ClientID:"oaiapp_test",AccessToken:"expired",RefreshToken:"saved-refresh",Scopes:granted(),ExpiresAt:time.Now().Add(-time.Hour)})
  mux.HandleFunc("/token",func(w http.ResponseWriter,r *http.Request){w.WriteHeader(400);_=json.NewEncoder(w).Encode(map[string]string{"error":code})})
  _,err:=m.AccessToken(context.Background(),"");if err==nil{t.Fatal("expected failure")};a,_:=m.accountLocked();if (a.RefreshToken=="")!=(code=="invalid_grant"){t.Fatal("incorrect credential retention")};if a.ClientID!="oaiapp_test"||a.Subject!="account-a"{t.Fatal("lost registered identity")}
 })}
}
func TestGeneric401RefreshDoesNotUseAPIKey(t *testing.T){
 m,mux:=fixture(t);t.Setenv("OPENAI_API_KEY","paid-api-secret");saveAccount(t,m,account{Subject:"account-a",ClientID:"oaiapp_test",AccessToken:"rejected",RefreshToken:"refresh",Scopes:granted(),ExpiresAt:time.Now().Add(time.Hour)})
 var requests atomic.Int32
 mux.HandleFunc("/token",func(w http.ResponseWriter,r *http.Request){_=json.NewEncoder(w).Encode(map[string]any{"access_token":"fresh","refresh_token":"next","expires_in":3600,"scope":scopes})})
 mux.HandleFunc("/v1/models",func(w http.ResponseWriter,r *http.Request){requests.Add(1);if r.Header.Get("Authorization")=="Bearer rejected"{w.WriteHeader(401);_,_=w.Write([]byte(`{"error":{"code":"invalid_token"}}`));return};if r.Header.Get("Authorization")!="Bearer fresh"{t.Error("wrong billing credential")};_,_=w.Write([]byte(`{"models":[{"slug":"gpt-visible","display_name":"Visible","visibility":"list"},{"slug":"hidden","visibility":"hide"}]}`))})
 models,err:=m.Models(context.Background(),"chatgpt");if err!=nil||len(models)!=1||models[0].ID!="gpt-visible"||requests.Load()!=2{t.Fatalf("model discovery %+v %v %d",models,err,requests.Load())}
}
func TestLogoutDuringExchangeCannotRestoreTokens(t *testing.T){
 m,mux:=fixture(t);entered:=make(chan struct{});release:=make(chan struct{});start,err:=m.BeginLogin(context.Background(),false);if err!=nil{t.Fatal(err)};u,_:=url.Parse(start.AuthorizationURL)
 mux.HandleFunc("/token",func(w http.ResponseWriter,r *http.Request){close(entered);<-release;_=json.NewEncoder(w).Encode(map[string]any{"access_token":"secret","refresh_token":"refresh","id_token":jwtFor(t,claimsFor(m,u.Query().Get("nonce"))),"scope":scopes,"expires_in":3600})})
 done:=make(chan error,1);go func(){_,err:=m.CompleteLogin(start.RedirectURI+"?"+url.Values{"state":{u.Query().Get("state")},"code":{"code"},"client_id":{"oaiapp_test"}}.Encode());done<-err}()
 <-entered;if err=m.Disconnect();err!=nil{t.Fatal(err)};close(release);if err=<-done;err==nil{t.Fatal("logout lost race")};s,_:=m.Status();if s.ChatGPTConnected{t.Fatal("credentials restored after logout")}
}
func TestProactiveRefreshOutageKeepsUnexpiredAccess(t *testing.T){
 m,mux:=fixture(t);saveAccount(t,m,account{Subject:"account-a",ClientID:"oaiapp_test",AccessToken:"still-valid",RefreshToken:"saved-refresh",Scopes:granted(),ExpiresAt:time.Now().Add(30*time.Second)})
 mux.HandleFunc("/token",func(w http.ResponseWriter,r *http.Request){w.WriteHeader(503);_,_=w.Write([]byte(`{"error":"server_error"}`))})
 token,err:=m.AccessToken(context.Background(),"");if err!=nil||token!="still-valid"{t.Fatalf("valid token unnecessarily lost: %q %v",token,err)}
 if _,err=m.AccessToken(context.Background(),"still-valid");err==nil{t.Fatal("explicitly rejected token must not be reused")}
}
