# Amplio: OpenAI Responses + ChatGPT subscription bridge

This is a **drop-in experimental bridge** for [google-deepmind/amplio](https://github.com/google-deepmind/amplio). It uses the existing `subprocess` provider without changing Amplio's Go core. The two modes are explicitly isolated:

| Mode | Backend | Credentials | Billing |
| --- | --- | --- | --- |
| `auth=api` | Public OpenAI Responses API | `OPENAI_API_KEY` | OpenAI API organization; eligible promotional API credits apply |
| `auth=chatgpt` | ChatGPT Codex subscription endpoint | Separate, official Codex CLI login | ChatGPT/Codex subscription allowance, **not** API credits |

## Copy into your Amplio fork

Copy the `bridges/openai_responses.py` and `scripts/login_chatgpt.sh` files into the matching paths in your Amplio checkout, then make them executable:

```bash
chmod +x bridges/openai_responses.py scripts/login_chatgpt.sh
python3 -m unittest discover -s tests -p 'test_openai_responses.py' -v
```

Amplio's subprocess bridge uses Unix sockets; use Linux, macOS or WSL rather than assuming native Windows compatibility. Build Amplio as described in upstream README (`make build`, Go and Node.js required).

## API credit mode

Set `OPENAI_API_KEY` in the shell where `amplio serve` runs. Choose the correct project/organization for your activated credits. Optional: `OPENAI_ORGANIZATION`, `OPENAI_PROJECT`. The bridge never stores these values in the fork.

Add to `~/.amplio/config.toml` (replace `/absolute/path/...` and the model ID with models available in your API project):

```toml
system_llm_hq = "subprocess{bin=/absolute/path/to/amplio/bridges/openai_responses.py}:YOUR_API_MODEL?auth=api"
system_llm_fast = "subprocess{bin=/absolute/path/to/amplio/bridges/openai_responses.py}:YOUR_FAST_API_MODEL?auth=api"

[run]
llms = [
  "subprocess{bin=/absolute/path/to/amplio/bridges/openai_responses.py}:YOUR_API_MODEL?auth=api&reasoning=high#API",
]
```

Usage is paid by the linked API organization. An activated promotional credit is applied before purchased credits. **This bridge has no credit-only hard stop:** if additional paid balance or auto-recharge is enabled, requests may continue after promotional credits run out. Check API billing configuration before unattended runs.

## ChatGPT subscription mode

Use the official Codex CLI for OAuth; *do not* copy your everyday `~/.codex/auth.json`. Create a separate `CODEX_HOME` for Amplio, so refresh-token rotation will not invalidate your normal Codex sessions:

```bash
./scripts/login_chatgpt.sh
```

It creates `~/.config/amplio-chatgpt/codex-home/config.toml` with `cli_auth_credentials_store = "file"` if missing, then runs the official `codex login`. The token lives at `~/.config/amplio-chatgpt/codex-home/auth.json` (mode `0600`), not inside the Git repo. If the Codex CLI changes its file format or credential handling, this bridge may need an update. To point to an alternate dedicated Codex directory, set `AMPLIO_CODEX_HOME` for both the login script and Amplio server; for an alternate credentials file, set `AMPLIO_CODEX_AUTH_FILE` for the server. Separate subprocesses coordinate refresh-token rotation using a file lock.

Add an independent model entry:

```toml
[run]
llms = [
  "subprocess{bin=/absolute/path/to/amplio/bridges/openai_responses.py}:YOUR_SUBSCRIPTION_MODEL?auth=chatgpt&reasoning=high#ChatGPT",
  "subprocess{bin=/absolute/path/to/amplio/bridges/openai_responses.py}:YOUR_API_MODEL?auth=api&reasoning=high#API",
]
```

Unlike API mode, the ChatGPT subscription backend is not a general, versioned public API; compatibility and available model IDs can change. The bridge obtains initial OAuth via Codex CLI and refreshes its *dedicated* token using the currently public Codex OAuth client. Verify both a real multi-turn tool call and an OAuth refresh before relying on it for long autonomous tasks.

## Protocol and state

- `POST /generate` follows Amplio's NDJSON protocol (`delta`, `final`, `error`).
- OpenAI Responses `function_call`/`function_call_output` items map to Amplio tool calls and tool results.
- Responses output items (including encrypted reasoning, tool calls, and assistant phase) are saved to `provider_extra` and replayed with the matching assistant turn; this follows the documented stateless reasoning pattern. API mode explicitly asks for `reasoning.encrypted_content`. No plain internal reasoning is deliberately exposed.
- `store: false` is used in both modes. Errors in one mode do not fall back to the other; this prevents accidental API spending.
- Only the shell process running Amplio gets the API key; ChatGPT refresh credentials stay in a private Codex home.
- OpenAI API and ChatGPT subscription usage/permissions follow different OpenAI policies.

## Validation status

Offline unit tests cover message/tool translation, stream event parsing, token refresh file rotation, and lack of unintended auth fallback. **No live model calls or end-to-end Amplio installation have been performed here.** First validate in an isolated workspace with a trivial shell/tool test, then a two-step tool sequence, then simulate a server restart.

Sources: [Amplio bridges protocol](https://github.com/google-deepmind/amplio/blob/main/bridges/README.md), [Amplio model configuration](https://github.com/google-deepmind/amplio/blob/main/docs/guide/models.md), [Cline ChatGPT OAuth implementation](https://github.com/cline/cline/blob/main/sdk/packages/core/src/auth/codex.ts), [OpenAI API credit benefit](https://help.openai.com/en/articles/20001626-api-credits-benefit-for-eligible-chatgpt-subscriptions).
