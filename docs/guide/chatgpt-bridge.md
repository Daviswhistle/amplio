# OpenAI Responses bridge: ChatGPT plan or API key

This optional **experimental** subprocess bridge connects Amplio to the public
OpenAI Responses API with two independently configured authentication modes.
Neither mode changes the Go core or falls back to another billing method.

| Auth mode | Credential | Endpoint | Accounting |
| --- | --- | --- | --- |
| chatgpt | Official open-source Sign in with ChatGPT (SIWC) OAuth | https://api.openai.com/v1/responses | Eligible ChatGPT plan |
| api | OPENAI_API_KEY | Same public endpoint | OpenAI API billing; eligible promotional credits may apply |

## Requirements and build

- Linux, macOS or WSL with Python 3.10+ (the Amplio subprocess transport uses
  Unix sockets, and credential refresh uses POSIX file locks).
- Build Amplio as usual with make build. The runtime bridge is Python standard
  library only. The *interactive sign-in CLI* additionally requires PyJWT with
  cryptographic signature verification:

    python3 -m pip install 'PyJWT[crypto]>=2.10,<3'

- Do not test agent shell execution on a machine containing sensitive files.
  Amplio does not sandbox the shell.

## Connect a ChatGPT plan (official dynamic OAuth registration)

First, authorize the app in a local browser:

    ./scripts/login_chatgpt.sh

The sign-in CLI uses the documented dynamic registration client, stable host
identifier, 127.0.0.1 callback, PKCE S256, state and nonce. It validates the
signed ID token against the public OpenAI JWKS (issuer, audience, expiration,
nonce and signature), checks the granted chatgpt.tokens.use.direct scope, and
saves the issued client ID for future sign-ins. A separate Codex CLI login,
Codex auth.json, secret client ID or private ChatGPT backend is not used.

Credentials are stored in the per-user directory
~/.config/amplio-chatgpt/siwc (owner-only 0700) and account.json (0600).
All token values stay outside Git, logs and run artifacts. Multiple Amplio
bridge subprocesses coordinate rotating refresh tokens with a file lock.
For a different ChatGPT account, explicitly run:

    ./scripts/login_chatgpt.sh --new-account

If sign-in succeeds but ChatGPT plan permission is declined, Amplio preserves
only the verified sign-in state, refuses inference, and requests explicit
re-consent on the next login attempt. It never switches to API billing.

The initial integration supports one active account per SIWC home. For multiple
isolated accounts, set a distinct AMPLIO_SIWC_HOME when running both the login
script and the Amplio server. During refresh, the issued client ID and the
Responses resource are supplied. Confirmed revocation stops plan inference;
temporary network failures do not erase the saved grant.

## Connect OpenAI API billing / eligible promotional credits

Export OPENAI_API_KEY in the environment that launches Amplio. Optional
OPENAI_ORGANIZATION and OPENAI_PROJECT select the intended API project.
Do not store API secrets in config.toml. The bridge does not enforce a
promotional-credit-only cutoff: charges may continue after credits run out.
Review billing limits and auto-recharge before unattended API-key runs.

## Amplio model configuration

Replace the example model IDs with ones available in your ChatGPT account
or API project. For account-specific model discovery, see OpenAI's SIWC
Models and inference documentation (https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference).

    system_llm_hq   = "subprocess{bin=/absolute/path/amplio/bridges/openai_responses.py}:gpt-6.1-sol?auth=chatgpt"
    system_llm_fast = "subprocess{bin=/absolute/path/amplio/bridges/openai_responses.py}:YOUR_FAST_API_MODEL?auth=api"

    [run]
    llms = [
      "subprocess{bin=/absolute/path/amplio/bridges/openai_responses.py}:gpt-6.1-sol?auth=chatgpt&reasoning=high#ChatGPT",
      "subprocess{bin=/absolute/path/amplio/bridges/openai_responses.py}:YOUR_API_MODEL?auth=api&reasoning=high#API",
    ]

The server process reads AMPLIO_SIWC_HOME and OPENAI_API_KEY independently.

## Protocol, failure handling and current limits

- Uses Amplio's NDJSON bridge protocol over a Unix socket. Complete responses,
  tool-call IDs, tool results, usage, and encrypted reasoning/output items
  are preserved and replayed as needed for stateless multi-turn requests.
- Both modes use stream=true, store=false and send full history. Amplio's
  system instructions are sent as the supported instructions field.
- SIWC plan requests group local function tools inside an amplio namespace
  and map namespaced responses back to the local tool registry. The currently
  unsupported SIWC request fields max_output_tokens and temperature are omitted
  for plan usage; API-key mode may send both.
- Ordinary rate limits and transient service failures are reported in the
  retryable form Amplio understands. ChatGPT plan usage-limit failures do not
  retry or switch to API billing. Only response.completed denotes success.
- Current integration uses one selected SIWC account per credential directory.
  No session data is sent to hosted conversation-storage endpoints.

## Offline verification

    python3 -m unittest discover -s tests -p 'test*.py' -v
    python3 -m py_compile bridges/openai_responses.py bridges/siwc_auth.py scripts/login_chatgpt.py
    bash -n scripts/login_chatgpt.sh

The offline tests exercise OAuth parameters, OIDC signature/audience/nonce
validation, permission scopes, file security and token rotation, response/tool
round-tripping, stream errors, HTTP 429 classification, and Unix socket framing.

**Not verified in an OpenAI account yet:** live SIWC browser registration,
real API-key/plan model calls, multi-agent server restart, and whether
promotional API credits pay for a specific request. Keep this integration
experimental until those checks pass.

## References

- OpenAI official open-source SIWC overview:
  https://developers.openai.com/siwc/token-sharing-open-source
- Registration and sign-in: https://developers.openai.com/siwc/token-sharing-open-source/sign-in
- Preview limitations: https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations
- Errors and recovery: https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery
- Amplio bridge protocol: https://github.com/google-deepmind/amplio/blob/main/bridges/README.md
