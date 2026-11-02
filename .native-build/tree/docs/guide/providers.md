# Providers: first run and account setup

Build Amplio with `make build`, then run **`./amplio`**. `amplio serve` remains
supported. Open the private URL printed by the program. A fresh data directory
opens **Providers** automatically; no config file, bootstrap script, Python,
pip, Codex CLI, or separate sign-in executable is required.

## Connect, choose, save

1. Choose **Sign in with ChatGPT** and complete authorization in the browser,
   or enter an OpenAI API key and choose **Save API key**. Both may be connected;
   each model explicitly selects one billing method. There is no automatic
   fallback from plan allowance to paid API usage.
2. Choose a model from the signed-in account's live catalog, or enter an exact
   supported model ID. **Model default** leaves reasoning controls to the model.
   **Test main model** makes one real inference request; catalog discovery and
   saving settings do not run inference. API catalog entries may include models
   that do not support Responses or tools; test the intended model before work.
3. Choose **Save and start using Amplio**. By default the selected model powers
   the main agent, reports/review and summaries/compaction. Enable separate
   models to choose each role explicitly. Settings apply without a server restart.

The main-agent default applies to **new runs**. Each run persists its concrete
agent model, so changing a default does not change existing agent sessions.
System review/summary models resolve the saved selection for their **next call**;
in-flight calls finish with their original model. Adding a model to the run
menu alone does not change these defaults.

## Browser login, including remote servers

ChatGPT OAuth is implemented inside the Go program: public SIWC registration,
PKCE, state/nonce, signed OIDC identity verification and automatic token refresh.
It uses the public Responses API, not private ChatGPT endpoints or another
application's tokens. Identity-only consent is preserved but cannot run models;
choose sign-in again to grant plan permission. Cancel, reconnect, switch account,
and remove local authorization are available in the same page.

OpenAI's public-client registration requires a `127.0.0.1` HTTP callback with
path `/auth/callback`. With a local browser it completes automatically. With a
remote browser, the final callback tab may show a connection error. Expand
**Using a remote server, or the browser could not return?**, copy the complete
callback URL from that tab, and paste it into **your own authenticated Amplio**.
The server validates the exact pending redirect, state, PKCE and account before
exchanging the one-use code. Do not send this address in chat, logs or tickets.
This is a manual remote completion path, not an unrestricted redirect or a
claim that every browser completes the loopback redirect automatically.

A sign-in attempt expires after five minutes. Provider denials, failed exchanges
and cancellation preserve an existing account. Disconnect cancels a pending
attempt and removes the local grant; provider-side revocation remains in
ChatGPT's account settings. One selected ChatGPT account is stored per data dir.

## Persistence and security

Model choices are saved in `<data-dir>/providers.json` without credentials.
An existing `config.toml` supplies initial defaults and other runtime settings;
it is never rewritten by this page. Explicit saved UI model choices override
those initial model defaults. A malformed settings file is an error, never a
reason to switch providers silently.

API keys are stored in `<data-dir>/credentials/openai_api_key`, ChatGPT grants
in `<data-dir>/credentials/chatgpt.json`, and the stable host ID alongside them.
Credential directories are 0700 and files 0600 on Unix; writes are atomic.
`OPENAI_API_KEY`, when supplied to the server, takes precedence over the saved
key and cannot be overwritten or deleted by the UI. `AMPLIO_OPENAI_KEY_FILE`
can point to an explicitly managed key file. Neither tokens nor saved keys are
returned to the browser, model specs, database events or application logs.

Provider endpoints require the Amplio owner token even for status reads, reject
cross-origin browser requests and require TLS for non-loopback clients. Use the
private startup URL only yourself. A reverse proxy should connect over TLS or
loopback. The native shell runs as the same OS account and is **not sandboxed**;
owner-only permissions do not isolate credentials from an agent running as you.
Use a separate account or container for untrusted tasks.

API calls may continue charging after promotional credits are exhausted. This
integration does not implement a credit-only spending cutoff. ChatGPT quota
exhaustion is terminal for the call and never switches to API billing.

## Migration from the draft Python bridge

The old `Connections` URL redirects to `Providers`. Stored draft specs pointing
to `openai_responses.py` are mapped to the native implementation at provider
construction; unrelated subprocess bridges are unchanged. Existing saved API
keys remain usable. Native ChatGPT credentials are intentionally separate from
the old Python/Codex token stores: sign in once through Providers rather than
letting two programs rotate the same refresh token. Old login/bootstrap scripts
and the Python runtime bridge have been removed.

Native specs are `chatgpt:MODEL` and `openai-responses:MODEL`, optionally followed
by `?reasoning.effort=high`. Model availability and accepted reasoning levels
are account/model dependent. `managed:hq` and `managed:fast` are internal lazy
system-role bindings, not agent choices. Headless commands retain their explicit
configuration requirements; zero-config onboarding is the web application flow.

## Verification boundary

The automated suite uses a fake OAuth issuer, signed JWTs, mock Responses SSE,
concurrent refreshes, real loopback callbacks and a real compiled Amplio web
server in Chromium. It verifies blank-directory setup, persistence, defaults,
restart and access boundaries without real OpenAI credentials or paid inference.
Live account eligibility, consent and model-specific tool behavior still require
a user-authorized account test. Offline tests do not establish those facts.

Official protocol references:
- https://developers.openai.com/siwc/token-sharing-open-source/sign-in
- https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference
- https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery
