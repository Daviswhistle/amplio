#!/usr/bin/env bash
# Copyright 2026 Google LLC
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA="${AMPLIO_DATA_DIR:-$HOME/.amplio}"
CONFIG="$DATA/config.toml"
if [[ -e "$CONFIG" ]]; then
  echo "Keeping existing $CONFIG; configure model tiers there if needed." >&2
  exit 0
fi
MODE="${1:---chatgpt}"
case "$MODE" in
  --chatgpt) AUTH='chatgpt' ;;
  --api) AUTH='api' ;;
  *) echo "Usage: $0 [--chatgpt|--api]" >&2; exit 2 ;;
esac
if [[ "$ROOT" == *'{'* || "$ROOT" == *'}'* || "$ROOT" == *'#'* || "$ROOT" == *'%'* ]]; then
  echo 'Path contains unsupported spec delimiters. Move checkout to a simpler path.' >&2
  exit 2
fi
mkdir -p "$DATA"
# Never overwrite existing operator configuration. The model can be changed from
# the UI for each run; system tiers require a restart after manual config edits.
MODEL="subprocess{bin=$ROOT/bridges/openai_responses.py}:gpt-6.1-sol?auth=$AUTH&reasoning=high"
( umask 077; cat > "$CONFIG" <<EOF
# Bootstrapped by scripts/bootstrap_openai.sh.
# The credential itself is managed in the web UI; no tokens go here.
system_llm_hq = "$MODEL"
system_llm_fast = "$MODEL"

[run]
llms = ["$MODEL"]
EOF
)
echo "Created $CONFIG with $AUTH as initial provider (model is editable for new runs in UI)."
echo 'Run ./amplio serve, open the printed URL, then visit Connections.'
