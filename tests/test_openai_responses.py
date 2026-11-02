"""Offline contract tests for the Amplio ChatGPT/Responses bridge."""
from __future__ import annotations

import importlib.util
import io
import http.client
import socket
import threading
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock
import sys
import urllib.error

SCRIPT = Path(__file__).resolve().parents[1] / "bridges" / "openai_responses.py"
sys.path.insert(0, str(SCRIPT.parent))
spec = importlib.util.spec_from_file_location("openai_responses_bridge", SCRIPT)
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class ResponseTests(unittest.TestCase):
    def setUp(self):
        self.request = {
            "model": "gpt-test", "max_tokens": 8192,
            "system_prompt": "You are a coding agent.",
            "messages": [
                {"role": "user", "content": "Fix the bug"},
                {"role": "assistant", "content": "", "tool_calls": [
                    {"id": "call_a", "name": "shell", "arguments": '{"command":"ls"}'},
                ]},
                {"role": "tool_result", "tool_call_id": "call_a", "content": "README.md"},
            ],
            "tools": [{"name": "shell", "description": "execute", "schema": {
                "type": "object", "properties": {"command": {"type": "string"}},
                "required": ["command"]}}],
        }

    def test_api_body_correct_and_explicit_caps(self):
        body = bridge.build_body(self.request, "api", "high")
        self.assertEqual(body["model"], "gpt-test")
        self.assertEqual(body["max_output_tokens"], 8192)
        self.assertEqual(body["include"], ["reasoning.encrypted_content"])
        self.assertEqual(body["reasoning"], {"effort": "high"})
        self.assertEqual(body["instructions"], "You are a coding agent.")
        self.assertEqual(body["input"][1]["call_id"], "call_a")
        self.assertEqual(body["input"][2]["type"], "function_call_output")
        self.assertEqual(body["tools"][0]["strict"], False)

    def test_subscription_body_drops_unsupported_fields(self):
        self.request["temperature"] = 0.4
        body = bridge.build_body(self.request, "chatgpt")
        self.assertNotIn("max_output_tokens", body)
        self.assertNotIn("temperature", body)
        self.assertEqual(body["include"], ["reasoning.encrypted_content"])
        self.assertFalse(body["store"])

    def test_disable_tools_retains_definitions(self):
        self.request["disable_tool_calls"] = True
        body = bridge.build_body(self.request, "chatgpt")
        self.assertEqual(body["tool_choice"], "none")
        self.assertEqual(len(body["tools"]), 1)
        self.assertEqual(body["tools"][0]["type"], "namespace")
        self.assertEqual(body["tools"][0]["name"], "amplio")
        self.assertEqual(body["tools"][0]["tools"][0]["name"], "shell")

    def test_namespace_tool_calls_are_mapped_back_to_amplio(self):
        output = bridge.parse_response({"status": "completed", "output": [
            {"type": "function_call", "call_id": "c1", "name": "amplio.shell", "arguments": "{}"}
        ]})
        self.assertEqual(output["tool_calls"][0]["name"], "shell")
        self.assertEqual(output["provider_extra"][bridge.OUTPUT_ITEMS_EXTRA_KEY][0]["name"], "amplio.shell")

    def test_opaque_reasoning_roundtrip(self):
        res = bridge.parse_response({"status": "completed", "output": [
            {"type": "reasoning", "id": "rs_1", "encrypted_content": "secretenc"},
            {"type": "function_call", "call_id": "call_b", "name": "shell", "arguments": "{}"}
        ], "usage": {"input_tokens": 20, "output_tokens": 7,
                     "input_tokens_details": {"cached_tokens": 11}}})
        self.assertEqual(res["tool_calls"][0]["id"], "call_b")
        self.assertEqual(res["usage"]["cache_read_tokens"], 11)
        self.assertEqual(res["stop_reason"], "tool_calls")
        self.assertEqual(len(res["provider_extra"][bridge.REASONING_EXTRA_KEY]), 1)
        nxt = {"model": "gpt-test", "messages": [{"role": "assistant", "content": "",
            "provider_extra": res["provider_extra"], "tool_calls": res["tool_calls"]}]}
        _, messages = bridge.build_input(nxt)
        self.assertEqual(messages[0]["type"], "reasoning")
        self.assertEqual(messages[1]["type"], "function_call")
        self.assertEqual(messages[0]["encrypted_content"], "secretenc")

    def test_assistant_phase_preserved(self):
        previous = bridge.parse_response({"output": [{"type": "message", "phase": "commentary",
            "status": "completed", "id": "msg_1", "role": "assistant", "content": [
                {"type": "output_text", "text": "Investigating..."}]}]})
        _, new_input = bridge.build_input({"messages": [{"role": "assistant", "content": previous["content"],
            "provider_extra": previous["provider_extra"]}]})
        self.assertEqual(new_input[0]["phase"], "commentary")
        self.assertEqual(len(new_input), 1)

    def test_refusal_and_incomplete(self):
        refusal = bridge.parse_response({"output": [{"type": "message", "content": [
            {"type": "refusal", "refusal": "Cannot do that"}]}]})
        self.assertEqual(refusal["stop_reason"], "refusal")
        self.assertEqual(refusal["refusal"]["category"], "refusal")
        incomplete = bridge.parse_response({"status": "incomplete", "incomplete_details": {
            "reason": "max_output_tokens"}})
        self.assertEqual(incomplete["stop_reason"], "max_tokens")

    def test_images(self):
        req = {"messages": [{"role": "user", "content": "Look", "attachments": [{
            "mime_type": "image/png", "base64_data": "ZmFrZQ=="}]}]}
        _, messages = bridge.build_input(req)
        self.assertEqual(messages[0]["content"][0]["text"], "Look")
        self.assertTrue(messages[0]["content"][1]["image_url"].startswith("data:image/png;base64,"))

    def test_multiline_sse_and_end(self):
        stream = io.BytesIO(b': keepalive\n\ndata: {"type": "response.completed",\ndata: "response": {"output": []}}\n\n')
        self.assertEqual(list(bridge._sse_events(stream))[0]["type"], "response.completed")

    def test_realistic_stream_tool_output(self):
        def event(data):
            return ("data: " + json.dumps(data) + "\n\n").encode()
        response = io.BytesIO(b"".join([
            event({"type": "response.output_item.added", "item_id": "fc_1", "item": {
                "id": "fc_1", "call_id": "call_1", "name": "shell", "type": "function_call"}}),
            event({"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": '{"cmd"'}),
            event({"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": ':"ls"}'}),
            event({"type": "response.completed", "response": {"output": [{
                "type": "function_call", "call_id": "call_1", "name": "shell",
                "arguments": '{"cmd":"ls"}'}], "usage": {"input_tokens": 7, "output_tokens": 8}}}),
        ]))
        with mock.patch.object(bridge, "auth_headers", return_value=(bridge.API_ENDPOINT, {})), \
             mock.patch.object(bridge.urllib.request, "urlopen", return_value=response):
            events = list(bridge.generate({"model": "x", "messages": []}, "api", ""))
        self.assertEqual(events[0]["tool_call_start"]["id"], "call_1")
        self.assertEqual(events[1]["tool_call_delta"]["arguments_delta"], '{"cmd"')
        self.assertEqual(events[-1]["type"], "final")
        self.assertEqual(events[-1]["response"]["tool_calls"][0]["arguments"], '{"cmd":"ls"}')

    def test_unix_socket_wire_protocol(self):
        with tempfile.TemporaryDirectory() as tmp:
            socket_path = str(Path(tmp) / "bridge.sock")
            srv = bridge.UnixServer(socket_path, bridge.make_handler("api", ""))
            worker = threading.Thread(target=srv.serve_forever, daemon=True)
            def fake_generate(req, auth, reasoning):
                self.assertEqual(auth, "api")
                self.assertEqual(req["model"], "gpt-test")
                yield {"type": "delta", "text": "done"}
                yield {"type": "final", "response": {"content": "done", "usage": {
                    "prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                    "stop_reason": "stop"}}
            try:
                with mock.patch.object(bridge, "generate", side_effect=fake_generate):
                    worker.start()
                    client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                    client.connect(socket_path)
                    req = json.dumps({"model": "gpt-test", "messages": []}).encode()
                    client.sendall(b"POST /generate HTTP/1.0\r\nHost: localhost\r\nContent-Type: application/json\r\n"
                                   + f"Content-Length: {len(req)}\r\n\r\n".encode() + req)
                    wire = bytearray()
                    while True:
                        part = client.recv(8192)
                        if not part:
                            break
                        wire.extend(part)
                    client.close()
                    status, payload = bytes(wire).split(b"\r\n\r\n", 1)
                    self.assertIn(b"200", status)
                    messages = [json.loads(line) for line in payload.splitlines() if line]
                    self.assertEqual(messages[0]["type"], "delta")
                    self.assertEqual(messages[-1]["response"]["content"], "done")
            finally:
                srv.shutdown()
                srv.server_close()
                if worker.is_alive():
                    worker.join(timeout=2)

    def test_subscription_uses_official_public_endpoint(self):
        with mock.patch.dict(os.environ, {"OPENAI_API_KEY": "unrelated-private-api-key"}), \
             mock.patch.object(bridge.siwc_auth, "get_access_token", return_value="siwc-token"):
            endpoint, headers = bridge.auth_headers("chatgpt")
        self.assertEqual(endpoint, "https://api.openai.com/v1/responses")
        self.assertEqual(headers["Authorization"], "Bearer siwc-token")
        self.assertNotIn("ChatGPT-Account-Id", headers)
        self.assertNotIn("OpenAI-Project", headers)

    def test_no_implicit_fallback_to_api(self):
        with mock.patch.dict(os.environ, {"OPENAI_API_KEY": "unrelated-api-key"}), \
             mock.patch.object(bridge.siwc_auth, "get_access_token", side_effect=bridge.siwc_auth.AuthError("OAuth missing")):
            with self.assertRaisesRegex(bridge.siwc_auth.AuthError, "OAuth missing"):
                bridge.auth_headers("chatgpt")

    def test_http_429_retries_only_for_ordinary_rate_limits(self):
        def make_error(code):
            body = json.dumps({"error": {"code": code, "message": "token=secret-value"}}).encode()
            return urllib.error.HTTPError("https://api.openai.com/v1/responses", 429,
                                          "Too Many Requests", {"x-request-id": "req_123"}, io.BytesIO(body))
        normal, _ = bridge._http_failure(make_error("rate_limit_exceeded"), "api")
        self.assertIn("(code 429)", str(normal))
        self.assertNotIn("secret-value", str(normal))
        subscription, _ = bridge._http_failure(make_error("subscription_sharing_usage_limit_exceeded"), "chatgpt")
        self.assertIn("usage limit", str(subscription))
        self.assertNotIn("(code 429)", str(subscription))

    def test_retries_one_expired_authentication_and_does_not_switch_billing(self):
        err = urllib.error.HTTPError("https://api.openai.com/v1/responses", 401, "Unauthorized", {},
                                    io.BytesIO(b'{"error":{"code":"invalid_token"}}'))
        with mock.patch.object(bridge, "auth_headers", return_value=(bridge.API_ENDPOINT, {"Authorization": "Bearer bad"})), \
             mock.patch.object(bridge.urllib.request, "urlopen", side_effect=[err, io.BytesIO(b'ok')]) as opened, \
             mock.patch.object(bridge.siwc_auth, "get_access_token", return_value="new") as refreshed:
            result = bridge._open_responses(b'{}', "chatgpt")
        self.assertEqual(result.read(), b'ok')
        self.assertEqual(opened.call_count, 2)
        refreshed.assert_called_once_with(force_refresh=True, previous_token="bad")

    def test_invalid_user_401_is_not_retried(self):
        exc = urllib.error.HTTPError("https://api.openai.com/v1/responses", 401, "Unauthorized", {},
                                     io.BytesIO(b'{"error":{"code":"subscription_sharing_invalid_user"}}'))
        with mock.patch.object(bridge, "auth_headers", return_value=(bridge.API_ENDPOINT, {"Authorization": "Bearer old"})), \
             mock.patch.object(bridge.urllib.request, "urlopen", side_effect=exc) as opened, \
             mock.patch.object(bridge.siwc_auth, "get_access_token") as refreshed:
            with self.assertRaises(bridge.BridgeError):
                bridge._open_responses(b'{}', "chatgpt")
        opened.assert_called_once()
        refreshed.assert_not_called()

    def test_stream_incomplete_never_returns_success(self):
        evt = b'data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}\n\n'
        with mock.patch.object(bridge, "auth_headers", return_value=(bridge.API_ENDPOINT, {})), \
             mock.patch.object(bridge.urllib.request, "urlopen", return_value=io.BytesIO(evt)):
            with self.assertRaisesRegex(bridge.BridgeError, "incomplete"):
                list(bridge.generate({"model": "gpt-test"}, "api", ""))

    def test_stream_subscription_limit_never_retries(self):
        evt = b'data: {"type":"response.failed","response":{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}}\n\n'
        with mock.patch.object(bridge, "auth_headers", return_value=(bridge.API_ENDPOINT, {})), \
             mock.patch.object(bridge.urllib.request, "urlopen", return_value=io.BytesIO(evt)):
            with self.assertRaisesRegex(bridge.BridgeError, "usage limit") as ctx:
                list(bridge.generate({"model": "gpt-test"}, "chatgpt", ""))
        self.assertNotIn("(code 429)", str(ctx.exception))

if __name__ == "__main__":
    unittest.main()
