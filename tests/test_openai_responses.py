"""Offline contract tests for the Amplio ChatGPT/Responses bridge."""
from __future__ import annotations

import base64
import importlib.util
import io
import http.client
import socket
import threading
import json
import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

SCRIPT = Path(__file__).resolve().parents[1] / "bridges" / "openai_responses.py"
spec = importlib.util.spec_from_file_location("openai_responses_bridge", SCRIPT)
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


def make_jwt(payload):
    text = base64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip("=")
    return "fake." + text + ".sig"


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
        self.assertNotIn("include", body)
        self.assertFalse(body["store"])

    def test_disable_tools_retains_definitions(self):
        self.request["disable_tool_calls"] = True
        body = bridge.build_body(self.request, "chatgpt")
        self.assertEqual(body["tool_choice"], "none")
        self.assertEqual(len(body["tools"]), 1)

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

    def test_no_implicit_fallback_to_api(self):
        with mock.patch.dict(os.environ, {"OPENAI_API_KEY": "sk-not-for-subscription"}):
            with mock.patch.object(bridge, "chatgpt_credentials", side_effect=bridge.BridgeError("OAuth missing")):
                with self.assertRaisesRegex(bridge.BridgeError, "OAuth missing"):
                    bridge.auth_headers("chatgpt")

    def test_subscription_credentials_refresh_to_dedicated_file(self):
        now = int(time.time())
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "auth.json"
            access = make_jwt({"exp": now - 10, "https://api.openai.com/auth": {"chatgpt_account_id": "acct_1"}})
            updated = make_jwt({"exp": now + 3600, "https://api.openai.com/auth": {"chatgpt_account_id": "acct_1"}})
            path.write_text(json.dumps({"tokens": {"access_token": access, "refresh_token": "ref-test"}}))
            def refresh(previous):
                other = json.loads(json.dumps(previous))
                other["tokens"]["access_token"] = updated
                other["tokens"]["refresh_token"] = "ref-new"
                return other
            with mock.patch.dict(os.environ, {"AMPLIO_CODEX_AUTH_FILE": str(path)}), \
                 mock.patch.object(bridge, "_refresh_tokens", side_effect=refresh):
                token, account = bridge.chatgpt_credentials()
            self.assertEqual(token, updated)
            self.assertEqual(account, "acct_1")
            self.assertEqual(json.loads(path.read_text())["tokens"]["refresh_token"], "ref-new")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_custom_codex_home(self):
        now = int(time.time())
        with tempfile.TemporaryDirectory() as directory:
            auth_path = Path(directory) / "auth.json"
            token = make_jwt({"exp": now + 3600, "https://api.openai.com/auth": {
                "chatgpt_account_id": "acct_custom"}})
            auth_path.write_text(json.dumps({"tokens": {"access_token": token}}))
            with mock.patch.dict(os.environ, {"AMPLIO_CODEX_HOME": directory}, clear=True):
                actual_token, account = bridge.chatgpt_credentials()
            self.assertEqual(actual_token, token)
            self.assertEqual(account, "acct_custom")

    def test_unavailable_auth_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.dict(os.environ, {"AMPLIO_CODEX_AUTH_FILE": directory + "/none"}):
                with self.assertRaisesRegex(bridge.BridgeError, "Dedicated Codex login"):
                    bridge.chatgpt_credentials()


if __name__ == "__main__":
    unittest.main()
