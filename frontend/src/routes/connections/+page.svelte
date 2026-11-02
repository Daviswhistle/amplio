<!--
 Copyright 2026 Google LLC

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
-->

<script lang="ts">
    import { onMount } from 'svelte';
    import { auth } from '$lib/auth.svelte';
    import { api, errorText } from '$lib/api';
    import { loadModelMenu } from '$lib/modelMenu';
    import { pageTitle } from '$lib/title';
    import type { OpenAIConnectionStatus, TestLLMResult } from '$lib/types';

    let status = $state<OpenAIConnectionStatus | null>(null);
    let error = $state('');
    let notice = $state('');
    let busy = $state(false);
    let apiKey = $state('');
    let mode = $state<'chatgpt' | 'api'>('chatgpt');
    let model = $state('');
    let effort = $state('high');
    let modelSpec = $state('');
    let testResult = $state<TestLLMResult | null>(null);
    let modelBusy = $state(false);

    async function load() {
        try {
            status = await api.getOpenAIConnections();
            error = '';
        } catch (e) {
            error = errorText(e);
        }
    }

    $effect(() => {
        if (auth.ready && auth.authed) void load();
    });
    onMount(() => {
        // The OAuth callback lands in its own temporary loopback tab. Poll only
        // during an ongoing login, rather than leaving a busy loop all day.
        const timer = setInterval(() => {
            if (auth.authed && status?.login_pending) void load();
        }, 2500);
        return () => clearInterval(timer);
    });

    async function saveKey() {
        if (!apiKey.trim() || busy) return;
        busy = true; error = ''; notice = '';
        try {
            await api.setOpenAIAPIKey(apiKey);
            apiKey = '';
            notice = 'API key saved in an owner-only local file.';
            await load();
        } catch (e) { error = errorText(e); }
        finally { busy = false; }
    }

    async function removeKey() {
        if (busy || !confirm('Remove the stored API key? Existing API runs may fail on their next request.')) return;
        busy = true; error = ''; notice = '';
        try {
            await api.deleteOpenAIAPIKey();
            notice = 'Stored API key removed.';
            await load();
        } catch (e) { error = errorText(e); }
        finally { busy = false; }
    }

    async function disconnectChatGPT() {
        if (busy || !confirm('Disconnect ChatGPT from this Amplio server? Running plan-backed tasks may fail.')) return;
        busy = true; error = ''; notice = '';
        try {
            await api.disconnectChatGPT();
            notice = 'Local ChatGPT credentials removed. This does not revoke access at the provider.';
            await load();
        } catch (e) { error = errorText(e); }
        finally { busy = false; }
    }

    async function startLogin() {
        if (busy) return;
        // Creating the tab immediately avoids popup blockers after an async POST.
        const popup = window.open('about:blank', '_blank');
        if (popup) popup.opener = null;
        busy = true; error = ''; notice = '';
        try {
            const { authorization_url } = await api.beginChatGPTLogin();
            if (popup) popup.location.replace(authorization_url);
            else window.location.assign(authorization_url);
            notice = 'Complete authorization in the new tab; this page will detect the connection.';
            await load();
        } catch (e) {
            popup?.close();
            error = errorText(e);
        } finally { busy = false; }
    }

    const isReady = $derived(mode === 'chatgpt' ? status?.chatgpt_connected : status?.api_key_source !== 'none');
    const validModel = $derived(/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,100}$/.test(model.trim()));
    const spec = $derived(
        status?.bridge_path && validModel
            ? `subprocess{bin=${encodeURIComponent(status.bridge_path)}}:${model.trim()}?auth=${mode}&reasoning=${effort}#${mode === 'chatgpt' ? 'ChatGPT' : 'OpenAI API'}`
            : ''
    );

    async function addModel() {
        if (!spec || modelBusy) return;
        modelBusy = true; error = ''; notice = ''; testResult = null;
        try {
            await api.addModel(spec);
            await loadModelMenu(true);
            modelSpec = spec;
            notice = 'Model added to the New Run selector.';
        } catch (e) { error = errorText(e); }
        finally { modelBusy = false; }
    }

    async function testModel() {
        if (!spec || modelBusy) return;
        modelBusy = true; error = ''; notice = ''; testResult = null;
        try {
            testResult = await api.testLLM(spec);
        } catch (e) { error = errorText(e); }
        finally { modelBusy = false; }
    }
</script>

<svelte:head><title>{pageTitle('Connections')}</title></svelte:head>

<div class="connections">
    <h2>Model connections</h2>
    <p class="dim">Link a ChatGPT plan or store an OpenAI API key on this server. The two methods never fall back to one another.</p>
    {#if auth.ready && !auth.authed}
        <section class="panel"><p>Owner sign-in is required to manage credentials.</p></section>
    {:else if !status}
        <section class="panel"><p class="dim">Loading connection status…</p></section>
    {:else}
        <section class="panel">
            <div class="section-head">
                <h3>ChatGPT plan</h3>
                <span class:ok={status.chatgpt_connected} class="pill">{status.chatgpt_connected ? 'Connected' : 'Not connected'}</span>
            </div>
            {#if status.chatgpt_email}<p class="dim small">{status.chatgpt_email}</p>{/if}
            <p class="dim small">Sign in with ChatGPT in a browser. The callback currently requires the browser to run on the Amplio host (or its loopback port to be forwarded).</p>
            <button class="primary" onclick={startLogin} disabled={busy || status.login_pending}>
                {status.login_pending ? 'Authorization in progress…' : status.chatgpt_connected ? 'Reconnect ChatGPT' : 'Sign in with ChatGPT'}
            </button>
            {#if status.chatgpt_connected}<button onclick={disconnectChatGPT} disabled={busy || status.login_pending}>Disconnect local account</button>{/if}
        </section>

        <section class="panel">
            <div class="section-head">
                <h3>OpenAI API</h3>
                <span class:ok={status.api_key_source !== 'none'} class="pill">{status.api_key_source === 'none' ? 'Not connected' : status.api_key_source === 'environment' ? 'Environment key' : 'Key saved'}</span>
            </div>
            <p class="dim small">API usage is billed separately. Promotional credits may apply, but this connection does not stop at a credit balance of zero.</p>
            {#if status.api_key_source === 'environment'}
                <p class="dim small">OPENAI_API_KEY is managed by the server process. Change it there.</p>
            {:else}
                <label for="api-key">API key</label>
                <div class="inline">
                    <input id="api-key" type="password" autocomplete="off" spellcheck="false" bind:value={apiKey} placeholder="sk-…" />
                    <button class="primary" onclick={saveKey} disabled={busy || !apiKey.trim()}>Save key</button>
                    {#if status.api_key_source === 'stored'}
                        <button onclick={removeKey} disabled={busy}>Remove</button>
                    {/if}
                </div>
                <p class="dim small">Stored locally with owner-only permissions; never returned to the browser.</p>
            {/if}
        </section>

        <section class="panel">
            <h3>Add & test a model</h3>
            <div class="fields">
                <label for="method">Connection</label>
                <select id="method" bind:value={mode}>
                    <option value="chatgpt">ChatGPT plan</option>
                    <option value="api">OpenAI API key</option>
                </select>
                <label for="model">Model ID</label>
                <input id="model" bind:value={model} placeholder="Model available to your account" autocomplete="off" spellcheck="false" />
                <label for="effort">Reasoning</label>
                <select id="effort" bind:value={effort}>
                    <option value="low">Low</option>
                    <option value="medium">Medium</option>
                    <option value="high">High</option>
                    <option value="xhigh">Extra high</option>
                </select>
            </div>
            <p class="dim small">Model IDs depend on your account. A connection test makes one real, potentially billable request.</p>
            <div class="inline buttons">
                <button class="primary" onclick={addModel} disabled={!isReady || !spec || modelBusy}>Add to model menu</button>
                <button onclick={testModel} disabled={!isReady || !spec || modelBusy}>{modelBusy ? 'Working…' : 'Test connection'}</button>
            </div>
            {#if modelSpec}
                <p class="dim small">Last added: <code>{modelSpec}</code></p>
            {/if}
            {#if testResult}
                <p class:ok={testResult.ok} class:fail={!testResult.ok} class="small">
                    {testResult.ok ? `✓ Connected (${testResult.latency_ms}ms)` : `✗ ${testResult.error || 'Model call failed'}`}
                </p>
            {/if}
            {#if !status.bridge_path}<p class="fail small">OpenAI bridge not found. Set AMPLIO_OPENAI_BRIDGE on the server.</p>{/if}
        </section>
    {/if}
    {#if error}<p class="fail small" role="alert">{error}</p>{/if}
    {#if notice}<p class="ok small" role="status">{notice}</p>{/if}
</div>

<style>
    .connections { max-width: 48rem; margin: 0 auto; padding: 1.2rem; display: flex; flex-direction: column; gap: 1rem; }
    h2, h3 { margin: 0; }
    h2 { font-size: var(--fs-xl); }
    h3 { font-size: var(--fs-lg); }
    .panel { padding: 1.2rem; background: var(--bg-elev); border: 1px solid var(--border); border-radius: var(--radius-md); display: flex; flex-direction: column; gap: 0.8rem; }
    .section-head { display: flex; justify-content: space-between; align-items: center; gap: 0.6rem; }
    .dim { color: var(--text-dim); }
    .small { font-size: var(--fs-sm); }
    .pill { background: var(--bg-elev2); padding: 0.18rem 0.6rem; border-radius: var(--radius-pill); font-size: var(--fs-sm); }
    .pill.ok { color: var(--ok); }
    .fields { display: grid; grid-template-columns: 7rem minmax(0,1fr); align-items: center; gap: 0.6rem; }
    .inline { display: flex; flex-wrap: wrap; gap: 0.5rem; }
    .inline input { flex: 1; min-width: 12rem; }
    input, select { font: inherit; padding: 0.45rem 0.65rem; border: 1px solid var(--border); border-radius: var(--radius-sm); color: var(--text); background: var(--bg-elev2); min-width: 0; }
    button { cursor: pointer; font: inherit; padding: 0.4rem 0.75rem; border-radius: var(--radius-sm); border: 1px solid var(--border); color: var(--text); background: var(--bg-elev2); }
    button.primary { background: var(--accent); border-color: var(--accent); color: var(--bg); }
    button:disabled { opacity: 0.5; cursor: not-allowed; }
    .buttons { margin-top: 0.1rem; }
    .ok { color: var(--ok); }
    .fail { color: var(--err); }
    code { overflow-wrap: anywhere; }
    @media (max-width: 640px) { .connections { padding: 0; } .fields { grid-template-columns: 1fr; gap: 0.3rem; } }
</style>
