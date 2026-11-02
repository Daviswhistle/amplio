// SPDX-License-Identifier: Apache-2.0
package main

import (
	"amplio/internal/config"
	"amplio/internal/llm"
	"amplio/internal/llm/responses"
	"amplio/internal/providerstate"
	"context"
	"errors"
	"github.com/spf13/cobra"
	"net/url"
	"os"
	"path/filepath"
	"sync"
)

// Native providers do not depend on Python, a checkout path or a second binary.
// managed is a lazy SYSTEM-model binding: it allows the UI to start before an
// account/model exists, and changes system defaults on their next call. Agent
// specs are resolved and persisted before a new run is created (server wrapper).
func init() {
	// Existing draft configurations and stored runs remain usable after the
	// Python adapter is removed. Other subprocess bridges are untouched.
	legacy := providerRegistry["subprocess"]
	providerRegistry["subprocess"] = providerEntry{clientArgs: legacy.clientArgs, new: func(model string, cap int, client, args url.Values) (llm.Provider, error) {
		if filepath.Base(client.Get("bin")) != "openai_responses.py" {
			return legacy.new(model, cap, client, args)
		}
		mode, options, err := legacyOpenAIArgs(args)
		if err != nil {
			return nil, err
		}
		return responses.New(providerstate.ForDir(config.DataDir()), mode, model, cap, options)
	}}
	providerRegistry["openai-responses"] = providerEntry{new: func(model string, cap int, _ url.Values, args url.Values) (llm.Provider, error) {
		return responses.New(providerstate.ForDir(config.DataDir()), "api", model, cap, args)
	}}
	providerRegistry["chatgpt"] = providerEntry{new: func(model string, cap int, _ url.Values, args url.Values) (llm.Provider, error) {
		return responses.New(providerstate.ForDir(config.DataDir()), "chatgpt", model, cap, args)
	}}
	providerRegistry["managed"] = providerEntry{new: func(role string, _ int, _ url.Values, args url.Values) (llm.Provider, error) {
		if (role != "hq" && role != "fast") || len(args) != 0 {
			return nil, errors.New("managed supports system roles hq and fast only")
		}
		return &managedModel{state: providerstate.ForDir(config.DataDir()), role: role}, nil
	}}
}

// Preserve flag > environment > config precedence. Only the web server may
// start with unresolved system tiers; headless execution still fails fast.
func resolveWebConfig(cmd *cobra.Command) (config.Config, error) {
	base, err := config.Load(config.DataDir())
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Resolve(config.DataDir(), config.Overrides{
		SystemLLMHQ:   firstNonEmpty(flagSystemLLMHQ, os.Getenv(config.EnvSystemLLMHQ), base.SystemLLMHQ, "managed:hq"),
		SystemLLMFast: firstNonEmpty(flagSystemLLMFast, os.Getenv(config.EnvSystemLLMFast), base.SystemLLMFast, "managed:fast"),
		EmbedModel:    flagEmbedModel, SkillDirs: flagSkillDirs, SkillDirsSet: cmd.Flags().Changed("skill-dir"), LessonSearch: lessonSearchOverride(cmd),
	})
	if err == nil {
		bridgeEndpoints = cfg.Bridge
	}
	return cfg, err
}
func prepareWebModels(cfg config.Config) (config.Config, *providerstate.Manager, error) {
	for i, spec := range cfg.Run.LLMs {
		cfg.Run.LLMs[i] = nativeSpec(spec)
	}
	cfg.SystemLLMHQ = nativeSpec(cfg.SystemLLMHQ)
	cfg.SystemLLMFast = nativeSpec(cfg.SystemLLMFast)
	state := providerstate.ForDir(config.DataDir())
	state.SetFallback(providerstate.Settings{Agent: cfg.DefaultLLM(), HQ: cfg.SystemLLMHQ, Fast: cfg.SystemLLMFast})
	if _, err := state.Settings(); err != nil {
		return cfg, nil, err
	}
	cfg.SystemLLMHQ = "managed:hq"
	cfg.SystemLLMFast = "managed:fast"
	return cfg, state, nil
}

type managedModel struct {
	state    *providerstate.Manager
	role     string
	mu       sync.Mutex
	spec     string
	provider llm.Provider
}

func (p *managedModel) resolve() (llm.Provider, error) {
	spec, err := p.state.Selected(p.role)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if spec != p.spec || p.provider == nil {
		next, err := createProvider(spec)
		if err != nil {
			return nil, err
		}
		p.provider = next
		p.spec = spec
	}
	return p.provider, nil
}
func (p *managedModel) ModelID() string {
	v, err := p.resolve()
	if err != nil {
		return "setup-required"
	}
	return v.ModelID()
}
func (p *managedModel) MaxTokens() int {
	v, err := p.resolve()
	if err != nil {
		return defaultMaxOutputTokens
	}
	return v.MaxTokens()
}
func (p *managedModel) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	v, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return v.Call(ctx, req)
}
func (p *managedModel) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	v, err := p.resolve()
	if err != nil {
		return nil, err
	}
	return v.Stream(ctx, req)
}
func legacyOpenAIArgs(args url.Values) (string, url.Values, error) {
	mode := "api"
	options := url.Values{}
	for k, vs := range args {
		if len(vs) != 1 {
			return "", nil, errors.New("duplicate legacy OpenAI argument")
		}
		switch k {
		case "auth":
			mode = vs[0]
		case "reasoning":
			if vs[0] != "" {
				options.Set("reasoning.effort", vs[0])
			}
		default:
			return "", nil, errors.New("unsupported legacy OpenAI argument; reselect the model in Providers")
		}
	}
	if mode != "api" && mode != "chatgpt" {
		return "", nil, errors.New("invalid legacy OpenAI authentication mode")
	}
	return mode, options, nil
}
func nativeSpec(spec string) string {
	sp, err := llm.ParseSpec(spec)
	if err != nil || sp.Provider != "subprocess" || filepath.Base(sp.Client.Get("bin")) != "openai_responses.py" {
		return spec
	}
	model, args, err := sp.Model()
	if err != nil {
		return spec
	}
	mode, options, err := legacyOpenAIArgs(args)
	if err != nil {
		return spec
	}
	sp.Provider = "openai-responses"
	if mode == "chatgpt" {
		sp.Provider = "chatgpt"
	}
	delete(sp.Client, "bin")
	sp.Tail = model
	if len(options) > 0 {
		sp.Tail += "?" + options.Encode()
	}
	return sp.String()
}
