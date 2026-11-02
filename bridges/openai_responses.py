#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Experimental integration for google-deepmind/amplio.
"""Amplio subprocess bridge for OpenAI Responses (API key or official SIWC).

Both modes use the public /v1/responses endpoint and Amplio's NDJSON bridge
protocol. ChatGPT credentials are issued by public dynamic OAuth registration,
not borrowed from the Codex CLI or any private backend.
"""

from __future__ import annotations

import argparse
import json
import os
import socketserver
import sys
import urllib.error
import urllib.request
from http import server as http_server
from typing import Any, Iterator

import siwc_auth

API_ENDPOINT = "https://api.openai.com/v1/responses"
REASONING_EXTRA_KEY = "openai_responses.reasoning_items"
OUTPUT_ITEMS_EXTRA_KEY = "openai_responses.output_items"
MAX_HTTP_BODY = 32 * 1024 * 1024


class BridgeError(siwc_auth.AuthError):
    """Safe provider or configuration failure."""


def auth_headers(auth: str) -> tuple[str, dict[str, str]]:
    headers = {"Content-Type": "application/json", "Accept": "text/event-stream",
               "User-Agent": "amplio-openai-responses-bridge/0.2"}
    if auth == "api":
        key = os.environ.get("OPENAI_API_KEY", "")
        if not key:
            raise BridgeError("OPENAI_API_KEY missing for API billing mode")
        headers["Authorization"] = "Bearer " + key
        if os.environ.get("OPENAI_ORGANIZATION"):
            headers["OpenAI-Organization"] = os.environ["OPENAI_ORGANIZATION"]
        if os.environ.get("OPENAI_PROJECT"):
            headers["OpenAI-Project"] = os.environ["OPENAI_PROJECT"]
        return API_ENDPOINT, headers
    if auth == "chatgpt":
        headers["Authorization"] = "Bearer " + siwc_auth.get_access_token()
        return API_ENDPOINT, headers
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


def _local_tool_name(name: str) -> str:
    """Translate the SIWC namespace into the local Amplio tool registry."""
    return name.removeprefix("amplio.")


def build_body(req: dict[str, Any], auth: str, reasoning: str = "") -> dict[str, Any]:
    instructions, messages = build_input(req)
    body: dict[str, Any] = {"model": req["model"], "input": messages,
                            "stream": True, "store": False}
    if instructions:
        body["instructions"] = instructions
    if req.get("tools"):
        definitions = [
            {"type": "function", "name": tool["name"],
             "description": tool.get("description", ""),
             "parameters": tool.get("schema") or {"type": "object", "properties": {}},
             "strict": False}
            for tool in req["tools"]
        ]
        # The SIWC preview requires function tools to be inside a namespace.
        # Keep API-key mode's regular flat function declarations unchanged.
        body["tools"] = ([{"type": "namespace", "name": "amplio",
                           "description": "Local tools controlled by the Amplio harness",
                           "tools": definitions}]
                         if auth == "chatgpt" else definitions)
        if req.get("disable_tool_calls"):
            body["tool_choice"] = "none"
    if auth == "api":
        if int(req.get("max_tokens", 0)) > 0:
            body["max_output_tokens"] = int(req["max_tokens"])
        if req.get("temperature") is not None:
            body["temperature"] = req["temperature"]
    # Encrypted reasoning items preserve stateless multi-turn continuity.
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
                               "name": _local_tool_name(item.get("name", "")),
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



NO_RETRY_ERRORS = {
    "subscription_sharing_user_not_eligible", "subscription_sharing_usage_limit_exceeded",
    "subscription_sharing_unsupported_capability", "subscription_sharing_route_not_supported",
    "subscription_sharing_invalid_user", "chatpass_v2_scope_not_authorized",
    "chatpass_v2_invalid_authorization_context", "invalid_api_key", "insufficient_quota",
}


def _safe_error_code(value: Any) -> str:
    if not isinstance(value, str) or len(value) > 100:
        return "request_failed"
    return value if value and all(x.isalnum() or x in "_-" for x in value) else "request_failed"


def _http_failure(exc: urllib.error.HTTPError, auth: str) -> tuple[BridgeError, str]:
    """Retain only safe code and request id. Do not echo error messages or tokens."""
    request_id = exc.headers.get("x-request-id", "") if exc.headers else ""
    if not (len(request_id) <= 120 and all(x.isalnum() or x in "_-" for x in request_id)):
        request_id = ""
    try:
        payload = json.loads(exc.read(4096))
    except (ValueError, AttributeError, TypeError):
        payload = {}
    error = payload.get("error") if isinstance(payload, dict) else None
    error = error if isinstance(error, dict) else {}
    code = _safe_error_code(error.get("code") or error.get("type"))
    if auth == "chatgpt" and code == "subscription_sharing_usage_limit_exceeded":
        return BridgeError("ChatGPT app usage limit reached; see ChatGPT Settings > Usage (subscription_sharing_usage_limit_exceeded)"), code
    message = f"Responses HTTP {exc.code} ({code})"
    param = error.get("param")
    if isinstance(param, str) and 0 < len(param) <= 96 and all(x.isalnum() or x in "_- ." for x in param):
        message += f" param={param}"
    if request_id:
        message += f" request_id={request_id}"
    # Amplio's subprocess retry logic recognizes literal '(code NNN)' suffixes.
    # Never retry access-control, invalid input, subscription caps or quota errors.
    if exc.code in {429, 500, 502, 503, 504} and code not in NO_RETRY_ERRORS:
        message += f" (code {exc.code})"
    return BridgeError(message), code


def _open_responses(body: bytes, auth: str):
    """One forced SIWC refresh on generic 401, never a billing fallback."""
    for attempt in range(2):
        endpoint, headers = auth_headers(auth)
        req = urllib.request.Request(endpoint, data=body, headers=headers, method="POST")
        try:
            return urllib.request.urlopen(req, timeout=1800)
        except urllib.error.HTTPError as exc:
            failure, code = _http_failure(exc, auth)
            if (auth == "chatgpt" and attempt == 0 and exc.code == 401
                    and code not in {"subscription_sharing_invalid_user"}):
                try:
                    siwc_auth.get_access_token(force_refresh=True,
                                               previous_token=headers["Authorization"][7:])
                except siwc_auth.AuthError as refresh_error:
                    raise BridgeError(str(refresh_error)) from None
                continue
            raise failure from None
        except urllib.error.URLError as exc:
            raise BridgeError(f"Responses connection failed ({type(exc.reason).__name__})") from None
    raise BridgeError("Responses authentication failure after refresh")


def generate(req: dict[str, Any], auth: str, reasoning: str) -> Iterator[dict[str, Any]]:
    if not isinstance(req.get("model"), str) or not req.get("model"):
        raise BridgeError("Missing model in Amplio request")
    body = build_body(req, auth, reasoning)
    data = json.dumps(body, separators=(",", ":")).encode("utf-8")
    response = _open_responses(data, auth)
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
                            "id": call_id, "name": _local_tool_name(item.get("name", ""))}}
            elif kind == "response.function_call_arguments.delta" and event.get("delta"):
                call_id = item_id_to_call.get(event.get("item_id", "")) or event.get("call_id")
                if call_id:
                    yield {"type": "delta", "tool_call_delta": {
                        "id": call_id, "arguments_delta": event["delta"]}}
            elif kind == "response.completed":
                final = event.get("response")
                if not isinstance(final, dict) or final.get("status", "completed") != "completed":
                    raise BridgeError("Responses stream did not complete successfully")
                break
            elif kind == "response.incomplete":
                response_data = event.get("response") or {}
                reason = _safe_error_code((response_data.get("incomplete_details") or {}).get("reason"))
                raise BridgeError(f"Responses stream incomplete ({reason}); no successful final response")
            elif kind in {"response.failed", "error"}:
                detail = (event.get("response") or {}).get("error") or event.get("error") or {}
                code = _safe_error_code(detail.get("code")) if isinstance(detail, dict) else "request_failed"
                if auth == "chatgpt" and code == "subscription_sharing_usage_limit_exceeded":
                    raise BridgeError("ChatGPT app usage limit reached; see ChatGPT Settings > Usage (subscription_sharing_usage_limit_exceeded)")
                suffix = " (code 503)" if code in {"subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable"} else ""
                raise BridgeError(f"Responses stream failed ({code}){suffix}")
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
            except (siwc_auth.AuthError, OSError, KeyError, TypeError, ValueError) as exc:
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
