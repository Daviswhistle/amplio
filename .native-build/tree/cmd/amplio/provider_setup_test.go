// SPDX-License-Identifier: Apache-2.0
package main
import (
 "context"
 "errors"
 "os"
 "path/filepath"
 "testing"
 "amplio/internal/config"
 "amplio/internal/llm"
 "amplio/internal/providerstate"
 "github.com/spf13/cobra"
)
func TestProviderSetupEmptyDirectory(t *testing.T){
 old:=config.DataDir();dir:=t.TempDir();config.SetDataDir(dir);t.Cleanup(func(){config.SetDataDir(old)});t.Setenv(config.EnvSystemLLMHQ,"");t.Setenv(config.EnvSystemLLMFast,"")
 cfg,err:=resolveWebConfig(&cobra.Command{});if err!=nil{t.Fatal(err)};cfg,manager,err:=prepareWebModels(cfg);if err!=nil{t.Fatal(err)};defer manager.Close()
 if cfg.SystemLLMHQ!="managed:hq"||cfg.SystemLLMFast!="managed:fast"{t.Fatalf("lazy system config: %+v",cfg)}
 provider,err:=createProvider(cfg.SystemLLMHQ);if err!=nil{t.Fatal(err)}
 if _,err=provider.Call(context.Background(),llm.Request{});!errors.Is(err,providerstate.ErrNotConfigured){t.Fatalf("unconfigured call: %v",err)}
 if err=manager.SaveSettings(providerstate.Settings{Agent:"openai-responses:unit-model",HQ:"openai-responses:unit-hq",Fast:"chatgpt:unit-fast"});err!=nil{t.Fatal(err)}
 if provider.ModelID()!="unit-hq"{t.Fatalf("saved settings not applied: %s",provider.ModelID())}
 if err=manager.SaveSettings(providerstate.Settings{Agent:"openai-responses:unit-next",HQ:"openai-responses:unit-hq-next",Fast:"chatgpt:unit-fast"});err!=nil{t.Fatal(err)}
 if provider.ModelID()!="unit-hq-next"{t.Fatal("system model did not update without restart")}
 if _,err:=os.Stat(filepath.Join(dir,"config.toml"));!os.IsNotExist(err){t.Fatal("unexpected config write")}
}
func TestProviderSetupKeepsExistingConfiguration(t *testing.T){
 old:=config.DataDir();dir:=t.TempDir();config.SetDataDir(dir);t.Cleanup(func(){config.SetDataDir(old)})
 cfg:=config.DefaultConfig();cfg.SystemLLMHQ="openai-responses:existing-hq";cfg.SystemLLMFast="openai-responses:existing-fast";cfg.Run.LLMs=[]string{"openai-responses:existing-agent"}
 _,m,err:=prepareWebModels(cfg);if err!=nil{t.Fatal(err)};defer m.Close();got,err:=m.Settings()
 if err!=nil||got.Agent!=cfg.DefaultLLM()||got.HQ!=cfg.SystemLLMHQ||got.Fast!=cfg.SystemLLMFast{t.Fatalf("fallback: %+v %v",got,err)}
}
func TestLegacyOpenAIBridgeMigratesWithoutPython(t *testing.T){
 old:=config.DataDir();config.SetDataDir(t.TempDir());t.Cleanup(func(){config.SetDataDir(old)})
 legacy:="subprocess{bin=/a/path/that/does/not/exist/openai_responses.py}:unit-model?auth=chatgpt&reasoning=high"
 if got:=nativeSpec(legacy);got!="chatgpt:unit-model?reasoning.effort=high"{t.Fatalf("native spec: %s",got)}
 p,err:=createProvider(legacy);if err!=nil||p.ModelID()!="unit-model"{t.Fatalf("legacy construction: %v",err)}
 other:="subprocess{bin=/other/custom.py}:model?option=x";if nativeSpec(other)!=other{t.Fatal("unrelated bridge modified")}
}
