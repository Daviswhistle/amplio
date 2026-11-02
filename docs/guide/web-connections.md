# Connect OpenAI from the Amplio web UI

The **Connections** page (`/connections`) is an experimental local setup flow for
OpenAI Responses. It requires the Amplio owner token and never displays existing
credentials or sends a key back to the browser.

## First-time setup

1. Build the fork with `make build` (Go version from `go.mod`, Node.js 22+).
2. Run `./scripts/bootstrap_openai.sh --chatgpt` to create a **new** `~/.amplio/config.toml`, or choose `--api` to use API billing for the initial system models. This script will not overwrite an existing config.
3. Run `python3 -m pip install 'PyJWT[crypto]>=2.10,<3'` if you want web-based ChatGPT sign-in.
4. Run `./amplio serve` and open the **token-bearing** URL printed by Amplio. Choose **Connections** in the navigation.
5. Sign in with ChatGPT (opens a new browser tab) and/or save the API key. Add a model to the run model selector and test it when ready.

The initial ChatGPT OAuth callback is a temporary HTTP loopback listener on the
**Amplio server host**. For this version, the browser must run on that host, or
its dynamically assigned callback port must be forwarded from the browser's
machine to the server. Remote web UI and mobile-browser login are not yet
supported end-to-end. CLI login (`./scripts/login_chatgpt.sh`) is still available.

The model field accepts an **exact model ID** supported by the chosen account.
The UI adds the corresponding subprocess spec to the **new-run model menu**; it
makes no API call until you explicitly use **Test connection** or start a run.
Adding a model does **not** switch the already-running system HQ/fast models or
change the server default in `config.toml`. Those require editing the file and
restarting Amplio. Model selection within each new run works immediately.

## Security and billing

- API keys entered through the UI are stored as an owner-only regular file at
  `<data-dir>/credentials/openai_api_key` (0600), in a 0700 directory. The Python
  bridge reads the file at request time via `AMPLIO_OPENAI_KEY_FILE`; API keys are
  not exported as inherited environment variables. Existing `OPENAI_API_KEY`
  environment settings take precedence and cannot be modified via the UI.
- All credential endpoints require the Amplio owner token; if server auth is
  disabled, credential management is disabled. Credential-changing requests
  reject cross-origin clients and non-TLS remote connections (loopback/SSH tunnel
  remains supported). Do not expose the token-bearing URL or serve credentials
  over untrusted networks. If TLS terminates at a reverse proxy, it must connect
  over loopback or use TLS to the Amplio backend.
- ChatGPT plan and API key modes stay separate. If plan access is denied or
  credits are exhausted, the bridge does **not** fall back to API billing.
- A successful **Test connection** makes a real inference call and may incur
  charges. The UI does not enforce a promotional-credit-only hard limit.
- Amplio's native agent shell is not sandboxed. An agent running as the same OS
  account may still access owner-readable files, including credentials. For
  untrusted tasks use a separate OS account or container.

## Validation status

Local offline tests cover the bridge, OAuth request and token contracts, owner
access, file permissions, key persistence/removal and browser-start framing.
The actual ChatGPT authorization, real model calls, full Amplio build and
frontend compiler checks require a connected environment and are **not**
verified by these tests. Do not mark this integration production-ready yet.

See [OpenAI Responses + ChatGPT bridge](chatgpt-bridge.md) for protocol details.
