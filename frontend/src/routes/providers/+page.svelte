<!-- SPDX-License-Identifier: Apache-2.0 -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { auth } from '$lib/auth.svelte';
  import { api } from '$lib/api';
  import { loadModelMenu } from '$lib/modelMenu';
  import { pageTitle } from '$lib/title';
  import { providers, modelSpec, parseChoice, type ProviderStatus, type ProviderID, type Choice, type ModelOption } from '$lib/providers';

  let status = $state<ProviderStatus | null>(null);
  let busy = $state(''); let error = $state(''); let notice = $state(''); let key = $state('');
  let callbackURL = $state(''); let loginURL = $state(''); let advanced = $state(false); let initialized = false;
  let choices = $state<Record<'agent' | 'hq' | 'fast', Choice>>({
    agent: { provider: 'chatgpt', model: '', effort: '' }, hq: { provider: 'chatgpt', model: '', effort: '' }, fast: { provider: 'chatgpt', model: '', effort: '' }
  });
  let catalogs = $state<Record<ProviderID, ModelOption[]>>({ chatgpt: [], openai: [] });
  let catalogError = $state<Record<ProviderID, string>>({ chatgpt: '', openai: '' });
  let loadingModels = $state<Record<ProviderID, boolean>>({ chatgpt: false, openai: false });
  const roles = [{ id: 'agent', title: 'Main agent' }, { id: 'hq', title: 'Review & reports' }, { id: 'fast', title: 'Summaries & compaction' }] as const;
  const shownRoles = $derived(advanced ? roles : roles.slice(0, 1));
  const valid = (c: Choice) => /^[A-Za-z0-9][A-Za-z0-9:._/-]{0,199}$/.test(c.model.trim());
  const connected = (id: ProviderID) => !!status && (id === 'chatgpt' ? status.chatgpt_connected : status.api_key_source !== 'none');
  const canSave = $derived(valid(choices.agent) && connected(choices.agent.provider) && (!advanced || (valid(choices.hq) && connected(choices.hq.provider) && valid(choices.fast) && connected(choices.fast.provider))));
  function receive(next: ProviderStatus) {
    const completedLogin = next.login_state === 'complete' && status?.login_state !== 'complete';
    const newlyConnected = next.chatgpt_connected && (!status?.chatgpt_connected || completedLogin);
    status = next;
    if (!initialized) {
      choices = { agent: parseChoice(next.settings.agent), hq: parseChoice(next.settings.hq), fast: parseChoice(next.settings.fast) };
      advanced = !!next.settings.agent && (next.settings.hq !== next.settings.agent || next.settings.fast !== next.settings.agent);
      initialized = true;
      if (next.api_key_source !== 'none') void loadModels('openai');
    }
    if (newlyConnected) { void loadModels('chatgpt'); loginURL = ''; callbackURL = ''; notice = 'ChatGPT is connected. Choose a model below.'; }
  }
  async function load() { try { receive(await providers.status()); } catch (e) { error = String(e); } }
  $effect(() => { if (auth.ready && auth.authed) void load(); });
  onMount(() => {
    let checking = false;
    const timer = setInterval(async () => {
      if (!status?.login_pending || checking) return;
      checking = true; try { await load(); } finally { checking = false; }
    }, 1500);
    return () => clearInterval(timer);
  });
  async function action(name: string, work: () => Promise<void>) {
    if (busy) return; busy = name; error = ''; notice = '';
    try { await work(); } catch (e) { error = String(e); } finally { busy = ''; }
  }
  async function loadModels(id: ProviderID) {
    if (!connected(id) || loadingModels[id]) return;
    loadingModels[id] = true; catalogError[id] = '';
    try { catalogs[id] = (await providers.models(id)).models; } catch (e) { catalogError[id] = String(e); } finally { loadingModels[id] = false; }
  }
  function changeProvider(role: 'agent' | 'hq' | 'fast') { choices[role].model = ''; choices[role].effort = ''; void loadModels(choices[role].provider); }
  function beginLogin(newAccount = false) {
    if (busy || status?.login_pending) return;
    const popup = window.open('about:blank', '_blank'); if (popup) popup.opener = null;
    void action('login', async () => {
      try {
        const login = await providers.login(newAccount); const target = new URL(login.authorization_url);
        if (target.protocol !== 'https:' || target.hostname !== 'auth.openai.com') throw new Error('Invalid sign-in destination');
        loginURL = login.authorization_url; if (popup) popup.location.replace(loginURL); await load();
        notice = popup ? 'Finish sign-in in the new tab. Amplio will detect the result.' : 'The browser blocked the new tab. Open the sign-in link below.';
      } catch (e) { popup?.close(); throw e; }
    });
  }
  function setKey() {
    void action('key', async () => {
      receive(await providers.saveKey(key)); key = ''; choices.agent.provider = 'openai'; choices.agent.model = ''; choices.agent.effort = '';
      notice = 'API key saved. Model discovery does not run inference.'; await loadModels('openai');
    });
  }
  function disconnectChatGPT() {
    if (!confirm('Remove this local ChatGPT authorization? Existing ChatGPT runs may fail on their next request.')) return;
    void action('disconnect', async () => { receive(await providers.disconnect()); catalogs.chatgpt = []; loginURL = ''; notice = 'Local authorization removed. Provider-side revocation is managed in ChatGPT settings.'; });
  }
  function removeKey() {
    if (!confirm('Remove the stored API key? Existing API runs may fail on their next request.')) return;
    void action('remove', async () => { receive(await providers.deleteKey()); catalogs.openai = []; notice = 'Stored API key removed.'; });
  }
  function saveDefaults() {
    if (!canSave) return;
    void action('save', async () => {
      const firstRun = status?.setup_required;
      receive(await providers.saveSettings({ agent: choices.agent, hq: advanced ? choices.hq : choices.agent, fast: advanced ? choices.fast : choices.agent }));
      await loadModelMenu(true); notice = 'Defaults saved and applied. Existing agent runs keep their selected model.';
      if (firstRun) await goto('/');
    });
  }
  function addModel() { void action('add', async () => { await providers.addModel(choices.agent); await loadModelMenu(true); notice = 'Model added to the New Run menu. Server defaults were not changed.'; }); }
  function testModel() {
    void action('test', async () => {
      const result = await api.testLLM(modelSpec(choices.agent)); if (!result.ok) throw new Error(result.error || 'Model test failed');
      notice = `Model responded successfully (${result.latency_ms} ms).`;
    });
  }
</script>
<svelte:head><title>{pageTitle('Providers')}</title></svelte:head>
<div class="providers">
  <header><p class="eyebrow">AMPLIO SETTINGS</p><h1>{status?.setup_required ? 'Welcome to Amplio' : 'Providers'}</h1><p class="dim">Connect an account, choose your models, and start working. No scripts or configuration files are required.</p></header>
  {#if error}<p class="message error" role="alert">{error}</p>{/if}
  {#if notice}<p class="message success" role="status">{notice}</p>{/if}
  {#if auth.ready && !auth.authed}
    <section class="panel"><h2>Owner access required</h2><p>Open the private URL printed by Amplio to manage providers.</p></section>
  {:else if !status}
    <section class="panel" aria-busy="true"><p>Loading provider settings…</p><button onclick={load}>Retry</button></section>
  {:else}
    <section aria-labelledby="accounts-heading">
      <h2 id="accounts-heading">1. Connect a provider</h2>
      <div class="accounts">
        <article class="panel">
          <div class="panel-heading"><h3>ChatGPT</h3><span class="badge" class:ready={status.chatgpt_connected}>{status.chatgpt_connected ? 'Connected' : status.chatgpt_signed_in ? 'Permission needed' : 'Not connected'}</span></div>
          <p class="dim">Use your eligible ChatGPT plan allowance. This never falls back to API billing.</p>
          {#if status.chatgpt_email}<p class="identity">{status.chatgpt_email}</p>{/if}
          {#if status.chatgpt_signed_in && !status.chatgpt_connected}<p>Identity is saved, but plan access is not enabled. Sign in again to authorize it.</p>{/if}
          <div class="buttons">
            <button class="primary" onclick={() => beginLogin()} disabled={!!busy || status.login_pending}>{status.login_pending ? 'Waiting for authorization…' : status.chatgpt_connected ? 'Reconnect ChatGPT' : 'Continue with ChatGPT'}</button>
            {#if status.chatgpt_signed_in}<button onclick={() => beginLogin(true)} disabled={!!busy || status.login_pending}>Switch account</button><button onclick={disconnectChatGPT} disabled={!!busy}>Disconnect</button>{/if}
          </div>
          {#if status.login_pending}
            <div class="pending" role="status"><p>Finish authorization in the browser. This attempt expires after five minutes.</p>
              {#if loginURL}<a href={loginURL} target="_blank" rel="noreferrer noopener">Open ChatGPT sign-in</a>{/if}
              <button onclick={() => action('cancel', async () => { receive(await providers.cancelLogin()); loginURL = ''; callbackURL = ''; })} disabled={!!busy}>Cancel sign-in</button>
            </div>
            <details><summary>Using a remote server, or the browser could not return?</summary>
              <p class="dim small">After authorization, copy the complete <code>http://127.0.0.1:…/auth/callback?…</code> address from that tab and paste it here. A connection error on that tab is expected for a remote server. Paste it only into your own Amplio instance.</p>
              <label for="callback-url">Authorization return URL</label><input id="callback-url" type="password" bind:value={callbackURL} autocomplete="off" spellcheck="false" />
              <button onclick={() => action('complete', async () => { receive(await providers.completeLogin(callbackURL)); callbackURL = ''; loginURL = ''; })} disabled={!!busy || !callbackURL.trim()}>Complete sign-in</button>
            </details>
          {/if}
          {#if status.login_error}<p class="error small" role="alert">{status.login_error}</p>{/if}
        </article>
        <article class="panel">
          <div class="panel-heading"><h3>OpenAI API</h3><span class="badge" class:ready={status.api_key_source !== 'none'}>{status.api_key_source === 'environment' ? 'Environment key' : status.api_key_source === 'stored' ? 'Key saved' : 'Not connected'}</span></div>
          <p class="dim">Separate API billing. Eligible credits may apply; usage can continue after credits are exhausted.</p>
          {#if status.api_key_source === 'environment'}<p class="small">This key is controlled by <code>OPENAI_API_KEY</code>. Amplio does not overwrite environment-owned credentials.</p>
          {:else}
            <form onsubmit={(event) => { event.preventDefault(); setKey(); }}>
              <label for="api-key">API key</label><input id="api-key" type="password" bind:value={key} autocomplete="off" spellcheck="false" placeholder="sk-…" />
              <div class="buttons"><button type="submit" class="primary" disabled={!!busy || !key.trim()}>Save API key</button>{#if status.api_key_source === 'stored'}<button type="button" onclick={removeKey} disabled={!!busy}>Remove key</button>{/if}</div>
            </form>
          {/if}
          <p class="dim small">Stored locally with owner-only file access. The saved key is never sent back to the browser.</p>
        </article>
      </div>
    </section>
    <section class="panel" aria-labelledby="models-heading">
      <h2 id="models-heading">2. Choose models</h2><p class="dim">Select from the current account catalog, or enter an exact model ID. No inference runs until you test a model or start work.</p>
      {#each shownRoles as role (role.id)}
        <fieldset><legend>{role.title}</legend><div class="model-fields">
          <label for={`${role.id}-provider`}>Provider<select id={`${role.id}-provider`} aria-label={`${role.title} provider`} bind:value={choices[role.id].provider} onchange={() => changeProvider(role.id)}><option value="chatgpt">ChatGPT plan</option><option value="openai">OpenAI API key</option></select></label>
          <label for={`${role.id}-model`}>Model<input id={`${role.id}-model`} aria-label={`${role.title} model`} list={`${role.id}-catalog`} bind:value={choices[role.id].model} autocomplete="off" spellcheck="false" placeholder={connected(choices[role.id].provider) ? 'Choose or enter a model ID' : 'Connect this provider first'} />
            <datalist id={`${role.id}-catalog`}>{#each catalogs[choices[role.id].provider] as model (model.id)}<option value={model.id}>{model.name}</option>{/each}</datalist>
          </label>
          <label for={`${role.id}-effort`}>Reasoning<select id={`${role.id}-effort`} aria-label={`${role.title} reasoning`} bind:value={choices[role.id].effort}><option value="">Model default</option><option value="low">Low</option><option value="medium">Medium</option><option value="high">High</option><option value="xhigh">Extra high</option><option value="max">Max</option></select></label>
        </div>
        <div class="catalog-row"><span class="dim small">{loadingModels[choices[role.id].provider] ? 'Loading account catalog…' : `${catalogs[choices[role.id].provider].length} account models`}</span><button class="small" onclick={() => loadModels(choices[role.id].provider)} disabled={!connected(choices[role.id].provider) || loadingModels[choices[role.id].provider]}>Refresh models</button></div>
        {#if catalogError[choices[role.id].provider]}<p class="error small">{catalogError[choices[role.id].provider]} You may still enter a known model ID manually.</p>{/if}
        </fieldset>
      {/each}
      <label class="checkbox"><input type="checkbox" bind:checked={advanced} /> Use different models for reviews and summaries</label>
      <p class="dim small">With this off, all three roles use the main agent's provider and model. Reasoning levels depend on the model; “Model default” avoids unsupported overrides.</p>
      <div class="buttons"><button onclick={testModel} disabled={!!busy || !valid(choices.agent) || !connected(choices.agent.provider)}>{busy === 'test' ? 'Testing…' : 'Test main model'}</button><button onclick={addModel} disabled={!!busy || !valid(choices.agent) || !connected(choices.agent.provider)}>Add to run menu only</button></div>
      <p class="dim small">A test makes one real model call and consumes the selected plan allowance or API credit/balance.</p>
    </section>
    <section class="panel finish"><div><h2>3. Save and use</h2><p class="dim small">The main model applies to new runs. Review and summary models apply on their next call. Existing agent runs are not restarted.</p></div><button class="primary" onclick={saveDefaults} disabled={!!busy || !canSave}>{busy === 'save' ? 'Saving…' : status.setup_required ? 'Save and start using Amplio' : 'Save defaults'}</button></section>
    <p class="dim small">Amplio executes tools with the server's operating-system account. Use an isolated account or container for untrusted tasks.</p>
  {/if}
</div>
<style>
  .providers { width:100%; max-width:68rem; margin:0 auto; display:flex; flex-direction:column; gap:1.4rem; }
  header { padding:.5rem 0; }.eyebrow { color:var(--text-dim); font-size:var(--fs-xs); letter-spacing:.12em; margin:0 0 .6rem; }
  h1 { font-size:1.8rem; margin:0 0 .65rem; } h2 { font-size:var(--fs-lg); margin:0 0 .85rem; } h3 { font-size:var(--fs-lg); margin:0; }
  p { line-height:1.6; margin:0 0 .7rem; } .dim { color:var(--text-dim); } .small { font-size:var(--fs-sm); }
  .accounts { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:1rem; }
  .panel { border:1px solid var(--border); border-radius:var(--radius-md); padding:1.3rem; background:var(--bg-elev); }
  .panel-heading { display:flex; flex-wrap:wrap; align-items:center; justify-content:space-between; gap:.5rem; margin-bottom:1rem; }
  .badge { font-size:var(--fs-xs); border:1px solid var(--border); border-radius:var(--radius-pill); padding:.2rem .6rem; color:var(--text-dim); }.badge.ready,.success { color:var(--ok); }
  .identity { overflow-wrap:anywhere; font-size:var(--fs-sm); }.buttons { display:flex; flex-wrap:wrap; gap:.6rem; margin:.7rem 0; }
  label { display:flex; flex-direction:column; gap:.4rem; font-size:var(--fs-sm); } input,select { color:var(--text); background:var(--bg-elev2); border:1px solid var(--border); border-radius:var(--radius-sm); padding:.65rem; font:inherit; min-width:0; width:100%; }
  input:focus,select:focus,button:focus-visible { outline:2px solid var(--accent); outline-offset:2px; }
  button { font:inherit; color:var(--text); background:var(--bg-elev2); border:1px solid var(--border); border-radius:var(--radius-sm); padding:.6rem .85rem; cursor:pointer; min-height:40px; }
  button.primary { color:var(--bg); background:var(--accent); border-color:var(--accent); } button:disabled { opacity:.5; cursor:not-allowed; }
  fieldset { border:1px solid var(--border); border-radius:var(--radius-sm); margin:1rem 0; padding:1rem; min-width:0; } legend { font-weight:600; padding:0 .4rem; }
  .model-fields { display:grid; grid-template-columns:1fr 1.6fr 1fr; gap:.75rem; }.catalog-row { display:flex; flex-wrap:wrap; align-items:center; justify-content:space-between; gap:.6rem; margin-top:.6rem; }
  .checkbox { display:flex; flex-direction:row; align-items:center; gap:.6rem; margin-bottom:.7rem; }.checkbox input { width:auto; }
  .finish { display:flex; align-items:center; gap:1.2rem; }.finish>div { flex:1; }.finish button { flex-shrink:0; }
  .message { padding:.8rem 1rem; background:var(--bg-elev); border:1px solid currentColor; border-radius:var(--radius-sm); overflow-wrap:anywhere; }.error { color:var(--err); }
  details { border-top:1px solid var(--border); margin-top:1rem; padding-top:.8rem; } summary { cursor:pointer; margin-bottom:.8rem; } details button { margin-top:.6rem; }.pending { margin-top:.7rem; }.pending a { margin-right:.7rem; }
  @media(max-width:760px) { .accounts,.model-fields { grid-template-columns:1fr; }.finish { flex-direction:column; align-items:stretch; }.panel { padding:1rem; } }
</style>
