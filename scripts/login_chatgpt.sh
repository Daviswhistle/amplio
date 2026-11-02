#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
exec python3 "$(dirname "${BASH_SOURCE[0]}")/login_chatgpt.py" "$@"
