#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Experimental integration for google-deepmind/amplio.
"""Amplio subprocess bridge for OpenAI Responses and ChatGPT Codex OAuth.

Python standard library only. The OAuth mode reads a *dedicated* Codex CLI
credential store made with CODEX_HOME=~/.config/amplio-chatgpt/codex-home codex login.
Do not share a single rotating refresh token store with a running Codex CLI.

Both modes use the Amplio /generate NDJSON protocol over a Unix socket.
See google-deepmind/amplio: bridges/README.md.
"""

from __future__ import annotations

import argparse
import base64
import datetime
import fcntl
import json
import os
import socketserver
import stat
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http import server as http_server
from pathlib import Path
from typing import Any, Iterator

API_ENDPOINT = "https://api.openai.com/v1/responses"
CHATGPT_ENDPOINT = "https://chatgpt.com/backend-api/codex/responses"
TOKEN_ENDPOINT = "https://auth.openai.com/oauth/token"
# The public Codex CLI OAuth client ID, as used by open-source Codex clients.
CODEX_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
REASONING_EXTRA_KEY = "openai_responses.reasoning_items"
OUTPUT_ITEMS_EXTRA_KEY = "openai_responses.output_items"
CREDENTIAL_LOCK = threading.Lock()
MAX_HTTP_BODY = 32 * 1024 * 1024


class BridgeError(Exception):
    """Safe, actionable provider or configuration failure."""


def _jwt_payload(token: str) -> dict[str, Any]:
    try:
        payload = token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        obj = json.loads(base64.urlsafe_b64decode(payload))
        return obj if isinstance(obj, dict) else {}
    except (IndexError, ValueError, TypeError):
        return {}


def _account_id(credentials: dict[str, Any]) -> str:
    tokens = credentials.get("tokens") or {}
    if tokens.get("account_id"):
        return str(tokens["account_id"])
    for name in ("id_token", "access_token"):
        jwt = _jwt_payload(tokens.get(name) or "")
        auth = jwt.get("https://api.openai.com/auth") or {}
        if auth.get("chatgpt_account_id"):
            return str(auth["chatgpt_account_id"])
        if jwt.get("chatgpt_account_id"):
            return str(jwt["chatgpt_account_id"])
    return ""


def _atomic_save_json(path: Path, content: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, tmp = tempfile.mkstemp(prefix=".oauth-", dir=str(path.parent))
    try:
        os.fchmod(fd, stat.S_IRUSR | stat.S_IWUSR)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(content, handle, ensure_ascii=False)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def _refresh_tokens(credentials: dict[str, Any]) -> dict[str, Any]:
    refresh_token = (credentials.get("tokens") or {}).get("refresh_token")
    if not refresh_token:
        raise BridgeError("ChatGPT OAuth refresh token missing; run dedicated 'codex login' again")
    body = urllib.parse.urlencode({
        "grant_type": "refresh_token", "client_id": CODEX_CLIENT_ID,
        "refresh_token": refresh_token,
    }).encode("utf-8")
    request = urllib.request.Request(TOKEN_ENDPOINT, data=body, headers={
        "Content-Type": "application/x-www-form-urlencoded",
    }, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            obj = json.load(response)
    except urllib.error.HTTPError as exc:
        # Never echo OAuth error bodies: they may include credential fragments.
        raise BridgeError(f"ChatGPT OAuth refresh failed (HTTP {exc.code}); re-run dedicated 'codex login'") from None
    except (urllib.error.URLError, ValueError) as exc:
        raise BridgeError(f"ChatGPT OAuth refresh unavailable: {type(exc).__name__}") from None
    if not isinstance(obj, dict) or not obj.get("access_token"):
        raise BridgeError("ChatGPT OAuth refresh did not return an access token")
    new_creds = dict(credentials)
    tokens = dict(new_creds.get("tokens") or {})
    tokens["access_token"] = obj["access_token"]
    if obj.get("refresh_token"):
        tokens["refresh_token"] = obj["refresh_token"]
    if obj.get("id_token"):
        tokens["id_token"] = obj["id_token"]
    new_creds["tokens"] = tokens
    new_creds["last_refresh"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    return new_creds


def chatgpt_credentials() -> tuple[str, str]:
    default_home = Path(os.environ.get("AMPLIO_CODEX_HOME", "~/.config/amplio-chatgpt/codex-home")).expanduser()
    path = Path(os.environ.get("AMPLIO_CODEX_AUTH_FILE") or default_home / "auth.json").expanduser()
    # Each model spec can spawn a distinct subprocess. A process-local lock
    # cannot protect rotating refresh tokens across those subprocesses.
    with CREDENTIAL_LOCK:
        lock_path = path.with_name(path.name + ".lock")
        lock_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        with lock_path.open("a+b") as lock_file:
            os.fchmod(lock_file.fileno(), stat.S_IRUSR | stat.S_IWUSR)
            fcntl.flock(lock_file.fileno(), fcntl.LOCK_EX)
            try:
                try:
                    data = json.loads(path.read_text(encoding="utf-8"))
                except (FileNotFoundError, ValueError) as exc:
                    raise BridgeError("Dedicated Codex login not found; see guide: ./scripts/login_chatgpt.sh") from exc
                tokens = data.get("tokens") or {}
                access = tokens.get("access_token") or ""
                if not access:
                    raise BridgeError("No ChatGPT subscription access token; dedicated Codex login must use ChatGPT, not an API key")
                expires_at = _jwt_payload(access).get("exp")
                if not isinstance(expires_at, (int, float)) or expires_at <= time.time() + 300:
                    data = _refresh_tokens(data)
                    _atomic_save_json(path, data)
                    access = data["tokens"]["access_token"]
                account_id = _account_id(data)
                if not account_id:
                    raise BridgeError("ChatGPT account ID absent from dedicated Codex login")
                return access, account_id
            finally:
                fcntl.flock(lock_file.fileno(), fcntl.LOCK_UN)


def auth_headers(auth: str) -> tuple[str, dict[str, str]]:
    headers = {"Content-Type": "application/json", "Accept": "text/event-stream",
               "User-Agent": "amplio-openai-responses-bridge/0.1"}
    if auth == "api":
        key = os.environ.get("OPENAI_API_KEY", "")
        if not key:
            raise BridgeError("OPENAI_API_KEY missing for API-credit mode")
        headers["Authorization"] = "Bearer " + key
        if os.environ.get("OPENAI_ORGANIZATION"):
            headers["OpenAI-Organization"] = os.environ["OPENAI_ORGANIZATION"]
        if os.environ.get("OPENAI_PROJECT"):
            headers["OpenAI-Project"] = os.environ["OPENAI_PROJECT"]
        return API_ENDPOINT, headers
    if auth == "chatgpt":
        access, account_id = chatgpt_credentials()
        headers.update({"Authorization": "Bearer " + access,
                        "ChatGPT-Account-Id": account_id,
                        "originator": "amplio"})
        return CHATGPT_ENDPOINT, headers
    raise BridgeError("Unknown auth mode; choose auth=api or auth=chatgpt")


def _input_part(content: str, attachments: list[dict[str, str]]) -> str | list[dict[str, str]]:
    if not attachments:
        return content
    parts: list[dict[str, str]] = []
    if content:
        parts.append({"type": "input_text", "text": content})
    for attachment in attachments:
        mime = attachment.get("mime_type", "image/png")
        if not mime.startswith("image/"):
            raise BridgeError("Unsupported attachment type; only images are accepted")
        image_data = attachment.get("base64_data", "")
        parts.append({"type": "input_image", "image_url": f"data:{mime};base64,{image_data}"})
    return parts


def build_input(req: dict[str, Any]) -> tuple[str, list[dict[str, Any]]]:
    instructions = [req.get("system_prompt", "")]
    messages: list[dict[str, Any]] = []
    for message in req.get("messages", []):
        role = message.get("role")
        content = message.get("content", "")
        attachments = message.get("attachments") or []
        if role == "system":
            instructions.append(content)
        elif role == "user":
            messages.append({"role": "user", "content": _input_part(content, attachments)})
        elif role == "assistant":
            extras = message.get("provider_extra") or {}
            raw_items = extras.get(OUTPUT_ITEMS_EXTRA_KEY)
            if isinstance(raw_items, list) and raw_items:
                # Replaying complete response items also preserves phase, encrypted
                # reasoning, call IDs, and future compatible output metadata.
                messages.extend(item for item in raw_items if isinstance(item, dict))
            else:
                # Fallback for existing Amplio turns with no provider extras.
                reasoning_items = extras.get(REASONING_EXTRA_KEY)
                if isinstance(reasoning_items, list):
                    messages.extend(item for item in reasoning_items if isinstance(item, dict)
                                    and item.get("type") == "reasoning")
                if content:
                    messages.append({"role": "assistant", "content": content})
                for call in message.get("tool_calls") or []:
                    if not call.get("id"):
                        raise BridgeError("Assistant tool call missing id")
                    messages.append({"type": "function_call", "call_id": call["id"],
                                     "name": call["name"], "arguments": call.get("arguments") or "{}"})
        elif role == "tool_result":
            call_id = message.get("tool_call_id")
            if not call_id:
                raise BridgeError("Tool result missing tool_call_id")
            messages.append({"type": "function_call_output", "call_id": call_id, "output": content})
            if attachments:
                messages.append({"role": "user", "content": _input_part("", attachments)})
        else:
            raise BridgeError(f"Unsupported Amplio role: {role!r}")
    return "\n\n".join(p for p in instructions if p), messages


def build_body(req: dict[str, Any], auth: str, reasoning: str = "") -> dict[str, Any]:
    instructions, messages = build_input(req)
    body: dict[str, Any] = {"model": req["model"], "input": messages,
                            "stream": True, "store": False}
    if instructions:
        body["instructions"] = instructions
    if req.get("tools"):
        body["tools"] = [
            {"type": "function", "name": tool["name"],
             "description": tool.get("description", ""),
             "parameters": tool.get("schema") or {"type": "object", "properties": {}},
             "strict": False}
            for tool in req["tools"]
        ]
        if req.get("disable_tool_calls"):
            body["tool_choice"] = "none"
    if auth == "api":
        if int(req.get("max_tokens", 0)) > 0:
            body["max_output_tokens"] = int(req["max_tokens"])
        if req.get("temperature") is not None:
            body["temperature"] = req["temperature"]
        body["include"] = ["reasoning.encrypted_content"]
    if reasoning:
        if reasoning not in {"low", "medium", "high", "xhigh"}:
            raise BridgeError("reasoning must be low, medium, high, or xhigh")
        body["reasoning"] = {"effort": reasoning}
    return body


def parse_response(data: dict[str, Any]) -> dict[str, Any]:
    output = data.get("output") or []
    text: list[str] = []
    tool_calls: list[dict[str, str]] = []
    reasoning: list[dict[str, Any]] = []
    refusal = None
    for item in output:
        kind = item.get("type")
        if kind == "reasoning" and item.get("encrypted_content"):
            reasoning.append({k: item[k] for k in ("id", "type", "encrypted_content", "summary") if k in item})
        elif kind == "function_call":
            tool_calls.append({"id": item.get("call_id") or item.get("id", ""),
                               "name": item.get("name", ""),
                               "arguments": item.get("arguments") or "{}"})
        elif kind == "message":
            for part in item.get("content") or []:
                if part.get("type") == "output_text":
                    text.append(part.get("text", ""))
                elif part.get("type") == "refusal":
                    refusal = {"category": "refusal", "explanation": part.get("refusal", "")}
    usage = data.get("usage") or {}
    prompt = int(usage.get("input_tokens") or 0)
    completion = int(usage.get("output_tokens") or 0)
    status = data.get("status", "completed")
    reason = ((data.get("incomplete_details") or {}).get("reason") if status == "incomplete" else None)
    stop_reason = "max_tokens" if reason == "max_output_tokens" else ("incomplete" if status == "incomplete" else ("refusal" if refusal else ("tool_calls" if tool_calls else "stop")))
    result: dict[str, Any] = {"content": "".join(text), "tool_calls": tool_calls,
        "usage": {"prompt_tokens": prompt, "completion_tokens": completion,
                  "total_tokens": int(usage.get("total_tokens") or prompt + completion),
                  "cache_read_tokens": int((usage.get("input_tokens_details") or {}).get("cached_tokens") or 0),
                  "cache_write_tokens": 0},
        "stop_reason": stop_reason}
    if refusal:
        result["refusal"] = refusal
    if output:
        # OpenAI recommends replaying *all* output items for stateless reasoning,
        # including phase markers. Amplio persists these only on assistant turns.
        result["provider_extra"] = {OUTPUT_ITEMS_EXTRA_KEY: output}
        if reasoning:
            result["provider_extra"][REASONING_EXTRA_KEY] = reasoning
    return result


def _sse_events(stream: Any) -> Iterator[dict[str, Any]]:
    """Combine multiline SSE data; ignore comments and unknown event types."""
    parts: list[str] = []
    for raw in stream:
        line = raw.decode("utf-8") if isinstance(raw, bytes) else raw
        line = line.rstrip("\r\n")
        if line == "":
            if parts:
                encoded = "\n".join(parts)
                parts.clear()
                if encoded == "[DONE]":
                    return
                try:
                    item = json.loads(encoded)
                except ValueError as exc:
                    raise BridgeError("Malformed provider SSE JSON") from exc
                if isinstance(item, dict):
                    yield item
            continue
        if line.startswith("data:"):
            parts.append(line[5:].lstrip(" "))
    if parts:
        if "\n".join(parts) != "[DONE]":
            raise BridgeError("Provider SSE stream ended mid-event")


def generate(req: dict[str, Any], auth: str, reasoning: str) -> Iterator[dict[str, Any]]:
    if not isinstance(req.get("model"), str) or not req.get("model"):
        raise BridgeError("Missing model in Amplio request")
    body = build_body(req, auth, reasoning)
    endpoint, headers = auth_headers(auth)
    data = json.dumps(body, separators=(",", ":")).encode("utf-8")
    request = urllib.request.Request(endpoint, data=data, headers=headers, method="POST")
    try:
        response = urllib.request.urlopen(request, timeout=1800)
    except urllib.error.HTTPError as exc:
        try:
            raw = exc.read(8192)
            payload = json.loads(raw)
            error = payload.get("error") or {}
            code = error.get("code") or error.get("type") or "request_failed"
            detail = error.get("message") or ""
            detail = detail[:600] if isinstance(detail, str) else ""
        except (ValueError, AttributeError, TypeError):
            code, detail = "request_failed", ""
        raise BridgeError(f"Responses API HTTP {exc.code} ({code}): {detail}") from None
    except urllib.error.URLError as exc:
        raise BridgeError(f"Responses API connection failed ({type(exc.reason).__name__})") from None
    item_id_to_call: dict[str, str] = {}
    final = None
    with response:
        for event in _sse_events(response):
            kind = event.get("type")
            if kind == "response.output_text.delta" and event.get("delta"):
                yield {"type": "delta", "text": event["delta"]}
            elif kind == "response.output_item.added":
                item = event.get("item") or {}
                if item.get("type") == "function_call":
                    call_id = item.get("call_id") or item.get("id")
                    if call_id:
                        item_id_to_call[event.get("item_id") or item.get("id", "")] = call_id
                        yield {"type": "delta", "tool_call_start": {
                            "id": call_id, "name": item.get("name", "")}}
            elif kind == "response.function_call_arguments.delta" and event.get("delta"):
                call_id = item_id_to_call.get(event.get("item_id", "")) or event.get("call_id")
                if call_id:
                    yield {"type": "delta", "tool_call_delta": {
                        "id": call_id, "arguments_delta": event["delta"]}}
            elif kind == "response.completed":
                final = event.get("response")
                break
            elif kind == "response.incomplete":
                final = event.get("response")
                break
            elif kind in {"response.failed", "error"}:
                error = (event.get("response") or {}).get("error") or event.get("error") or {}
                if isinstance(error, dict):
                    error = error.get("message") or error.get("code") or "unknown error"
                raise BridgeError(f"Responses API streaming failure: {str(error)[:400]}")
    if not isinstance(final, dict):
        raise BridgeError("Responses API ended without a completed or incomplete response")
    yield {"type": "final", "response": parse_response(final)}


class UnixServer(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True

    def server_bind(self):
        try:
            os.unlink(self.server_address)
        except FileNotFoundError:
            pass
        super().server_bind()
        os.chmod(self.server_address, 0o600)


def make_handler(auth: str, reasoning: str):
    class Handler(http_server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.0"

        def log_message(self, fmt, *args):
            # Never log prompts or authorization headers.
            pass

        def address_string(self):
            return "unix"

        def do_GET(self):
            if self.path != "/health":
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def do_POST(self):
            if self.path != "/generate":
                self.send_error(404)
                return
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if not (0 < size <= MAX_HTTP_BODY):
                    raise ValueError("Invalid request size")
                req = json.loads(self.rfile.read(size))
                if not isinstance(req, dict):
                    raise ValueError("Request must be a JSON object")
            except (ValueError, TypeError):
                self.send_error(400, "invalid request")
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.end_headers()
            try:
                for result in generate(req, auth, reasoning):
                    self.wfile.write((json.dumps(result, ensure_ascii=False) + "\n").encode("utf-8"))
                    self.wfile.flush()
            except (BridgeError, OSError, KeyError, TypeError, ValueError) as exc:
                try:
                    self.wfile.write((json.dumps({"type": "error", "error": str(exc)}) + "\n").encode("utf-8"))
                    self.wfile.flush()
                except OSError:
                    pass

    return Handler


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", required=True)
    args = parser.parse_args()
    spec = urllib.parse.parse_qs(os.environ.get("AMPLIO_BRIDGE_SPEC", ""))
    auth = (spec.get("auth") or ["api"])[0]
    reasoning = (spec.get("reasoning") or [""])[0]
    if auth not in {"api", "chatgpt"}:
        parser.error("auth must be api or chatgpt")
    if reasoning and reasoning not in {"low", "medium", "high", "xhigh"}:
        parser.error("reasoning must be low, medium, high, or xhigh")
    server = UnixServer(args.socket, make_handler(auth, reasoning))
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        try:
            os.unlink(args.socket)
        except FileNotFoundError:
            pass


if __name__ == "__main__":
    main()
