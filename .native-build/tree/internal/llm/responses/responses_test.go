// SPDX-License-Identifier: Apache-2.0
package responses
import (
 "amplio/internal/llm"
 "amplio/internal/providerstate"
 "context"
 "encoding/json"
 "errors"
 "io"
 "net/http"
 "net/url"
 "strings"
 "testing"
 "time"
)
func provider(t *testing.T,mode string)*Provider{t.Helper();p,e:=New(providerstate.ForDir(t.TempDir()),mode,"gpt-test",2048,url.Values{});if e!=nil{t.Fatal(e)};v:=p.(*Provider);v.pause=func(context.Context,time.Duration)error{return nil};return v}
func response(status int,body string)*http.Response{return &http.Response{StatusCode:status,Header:make(http.Header),Body:io.NopCloser(strings.NewReader(body))}}
func TestModeSpecificBodies(t *testing.T){
 for _,mode:=range []string{"api","chatgpt"}{t.Run(mode,func(t *testing.T){
  p:=provider(t,mode);temp:=0.5
  b,e:=p.body(llm.Request{SystemPrompt:"system",Temperature:&temp,DisableToolCalls:true,Messages:[]llm.Message{{Role:llm.RoleUser,Content:"hello"}},Tools:[]llm.ToolDef{{Name:"bash",Schema:json.RawMessage(`{"type":"object","properties":{}}`)}}});if e!=nil{t.Fatal(e)}
  var out map[string]any;_=json.Unmarshal(b,&out)
  if out["store"]!=false||out["stream"]!=true||out["tool_choice"]!="none"{t.Fatal(string(b))}
  _,cap:=out["max_output_tokens"];if cap!=(mode=="api"){t.Fatal("unsupported subscription cap")}
  tools:=out["tools"].([]any);first:=tools[0].(map[string]any);if (first["type"]=="namespace")!=(mode=="chatgpt"){t.Fatal("namespace handling")}
 })}
}
func TestToolAndOpaqueOutputReplay(t *testing.T){
 raw:=json.RawMessage(`{"status":"completed","output":[{"type":"reasoning","id":"r1","encrypted_content":"opaque","summary":[]},{"type":"function_call","id":"f1","call_id":"c1","namespace":"amplio","name":"bash","arguments":"{\"command\":\"true\"}"}],"usage":{"input_tokens":10,"output_tokens":3,"total_tokens":13,"input_tokens_details":{"cached_tokens":4}}}`)
 parsed,e:=parseResponse(raw);if e!=nil{t.Fatal(e)};if parsed.ToolCalls[0].ID!="c1"||parsed.Usage.CacheReadTokens!=4{t.Fatal(parsed)}
 b,e:=provider(t,"chatgpt").body(llm.Request{Messages:[]llm.Message{{Role:llm.RoleAssistant,ToolCalls:parsed.ToolCalls,ProviderExtra:parsed.ProviderExtra},{Role:llm.RoleToolResult,ToolCallID:"c1",Content:"done"}}});if e!=nil{t.Fatal(e)}
 var out struct{Input []map[string]any};_=json.Unmarshal(b,&out)
 if len(out.Input)!=3||out.Input[0]["encrypted_content"]!="opaque"||out.Input[2]["call_id"]!="c1"{t.Fatal(string(b))}
}
func TestHTTPRetryAndQuotaBoundary(t *testing.T){
 for _,code:=range []string{"rate_limit_exceeded","subscription_sharing_usage_limit_exceeded","insufficient_quota"}{t.Run(code,func(t *testing.T){
  p:=provider(t,"chatgpt");calls:=0;p.request=func(context.Context,string,string,string,[]byte)(*http.Response,error){calls++;return response(429,`{"error":{"code":"`+code+`"}}`),nil}
  _,e:=p.Call(context.Background(),llm.Request{});if e==nil{t.Fatal("failure expected")};want:=1;if code=="rate_limit_exceeded"{want=4};if calls!=want{t.Fatalf("retried %s %d times",code,calls)}
 })}
}
func TestStreamingTerminalStates(t *testing.T){
 good:="data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"
 for _,tc:=range []struct{name,body string;ok bool}{
  {"complete",good,true},{"empty","",false},{"done-only","data: [DONE]\n\n",false},
  {"truncated","data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",false},
  {"quota-midstream","data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\"}}}\n\n",false},
  {"incomplete","data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n",false},
 }{t.Run(tc.name,func(t *testing.T){p:=provider(t,"api");p.request=func(context.Context,string,string,string,[]byte)(*http.Response,error){return response(200,tc.body),nil};got,e:=p.Call(context.Background(),llm.Request{});if (e==nil)!=tc.ok{t.Fatalf("%s result=%+v err=%v",tc.name,got,e)}})}
}
func TestToolDeltaIdentity(t *testing.T){
 body:=`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"item1","call_id":"call1","name":"amplio.bash"}}

data: {"type":"response.function_call_arguments.delta","item_id":"item1","delta":"{}"}

data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call1","name":"amplio.bash","arguments":"{}"}]}}

`
 p:=provider(t,"chatgpt");p.request=func(context.Context,string,string,string,[]byte)(*http.Response,error){return response(200,body),nil};s,e:=p.Stream(context.Background(),llm.Request{});if e!=nil{t.Fatal(e)};defer s.Close()
 if !s.Next()||s.Event().ToolCallStart.Name!="bash"{t.Fatal("missing local tool start")}
 if !s.Next()||s.Event().ToolCallDelta.ID!="call1"{t.Fatal("delta ID changed")}
 if s.Next()||s.Err()!=nil||s.Response().ToolCalls[0].ID!="call1"{t.Fatal("final tool ID changed")}
}
func TestRejectMalformedHistoricalAndFinalTools(t *testing.T){
 p:=provider(t,"api");if _,e:=p.body(llm.Request{Messages:[]llm.Message{{Role:llm.RoleToolResult,Content:"orphan"}}});e==nil{t.Fatal("orphan tool result allowed")}
 for _,raw:=range []string{`{"status":"completed","output":[{"type":"function_call","name":"bash","arguments":"{}"}]}`,`{"status":"completed","output":[{"type":"function_call","call_id":"a","name":"bash","arguments":"{"}]}`} {if _,e:=parseResponse([]byte(raw));e==nil{t.Fatal("malformed final tool accepted")}}
}
func TestCancellationInterruptsRetry(t *testing.T){
 p:=provider(t,"api");ctx,cancel:=context.WithCancel(context.Background());p.pause=pause;p.request=func(context.Context,string,string,string,[]byte)(*http.Response,error){cancel();return response(503,`{"error":{"code":"temporarily_unavailable"}}`),nil}
 _,e:=p.Call(ctx,llm.Request{});if !errors.Is(e,context.Canceled){t.Fatal(e)}
}
