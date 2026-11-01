#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

if ! command -v codex >/dev/null 2>&1; then
  echo 'Codex CLI is required for the ChatGPT OAuth login: install it first.' >&2
  exit 1
fi

# Never touch the normal ~/.codex credentials or config. Use a dedicated home.
CODEX_HOME="${AMPLIO_CODEX_HOME:-$HOME/.config/amplio-chatgpt/codex-home}"
umask 077
mkdir -p "$CODEX_HOME"
if [[ ! -e "$CODEX_HOME/config.toml" ]]; then
  printf 'cli_auth_credentials_store = "file"\n' > "$CODEX_HOME/config.toml"
fi

echo "Signing in via official Codex CLI (isolated credentials: $CODEX_HOME)"
CODEX_HOME="$CODEX_HOME" codex login
if [[ ! -f "$CODEX_HOME/auth.json" ]]; then
  echo 'Expected auth.json was not created; check cli_auth_credentials_store=file.' >&2
  exit 1
fi
chmod 600 "$CODEX_HOME/auth.json"
echo 'ChatGPT login ready for Amplio (no API credits used by this mode).'
