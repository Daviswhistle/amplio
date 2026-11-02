#!/usr/bin/env python3
"""Register and authorize Amplio through public Sign in with ChatGPT (OSS)."""

from __future__ import annotations

import argparse
import base64
import datetime
import hashlib
import http.server
import json
import os
import secrets
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid
import webbrowser
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "bridges"))
import siwc_auth as auth

AUTHORIZE = "https://auth.openai.com/api/accounts/authorize"
JWKS = "https://auth.openai.com/.well-known/jwks.json"
SCOPES = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
CALLBACK_PATH = "/auth/callback"
CALLBACK_HOST = "127.0.0.1"


def host_id() -> str:
    auth._ensure_private_dir(auth.home())
    path = auth.home() / "host_id"
    try:
        return path.read_text(encoding="utf-8").strip()
    except FileNotFoundError:
        new_id = "urn:uuid:" + str(uuid.uuid4())
        try:
            flags = os.O_CREAT | os.O_EXCL | os.O_WRONLY
            if hasattr(os, "O_NOFOLLOW"):
                flags |= os.O_NOFOLLOW
            fd = os.open(path, flags, 0o600)
        except FileExistsError:
            return path.read_text(encoding="utf-8").strip()
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(new_id)
        return new_id


def authorize_params(issued_id: str | None, redirect: str, host: str,
                     state: str, nonce: str, verifier: str,
                     previous: dict | None) -> dict[str, str]:
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    result = {
        "client_id": issued_id or "dynamic_agent_client",
        "response_type": "code", "redirect_uri": redirect,
        "scope": SCOPES, "resource": auth.RESOURCE,
        "state": state, "nonce": nonce,
        "code_challenge_method": "S256", "code_challenge": challenge,
        "ext_agent_host_id": host,
    }
    if issued_id:
        if previous and previous.get("id_token"):
            result["id_token_hint"] = previous["id_token"]
        if previous and auth.PLAN_SCOPE not in previous.get("scopes", []):
            result["prompt"] = "consent"  # supported re-consent flow
    else:
        result["agent_name_hint"] = "Amplio"
    return result


def validate_id_token(id_token: str, client_id: str, nonce: str) -> dict:
    try:
        import jwt  # PyJWT[crypto] is needed only for the interactive login CLI.
        signing_key = jwt.PyJWKClient(JWKS).get_signing_key_from_jwt(id_token)
        claims = jwt.decode(id_token, signing_key.key, algorithms=["RS256"],
                            audience=client_id, issuer="https://auth.openai.com",
                            options={"require": ["exp", "iss", "aud", "sub", "nonce"]},
                            leeway=60)
        if claims.get("nonce") != nonce or not claims.get("sub"):
            raise auth.AuthError("ID token nonce or subject mismatch")
        return claims
    except ImportError as exc:
        raise auth.AuthError("Login needs PyJWT[crypto]; pip install 'PyJWT[crypto]>=2.10,<3'") from exc
    except auth.AuthError:
        raise
    except Exception as exc:
        # No JWT fragments or full authorization URL in diagnostics.
        raise auth.AuthError(f"ID token verification failed ({type(exc).__name__})") from None


def exchange_code(code: str, client_id: str, verifier: str, redirect: str) -> dict:
    form = urllib.parse.urlencode({
        "grant_type": "authorization_code", "client_id": client_id,
        "code": code, "code_verifier": verifier,
        "redirect_uri": redirect, "resource": auth.RESOURCE,
    }).encode("utf-8")
    request = urllib.request.Request(auth.TOKEN_ENDPOINT, data=form,
                                     headers={"Content-Type": "application/x-www-form-urlencoded"},
                                     method="POST")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            payload = json.load(response)
    except urllib.error.HTTPError as exc:
        raise auth.AuthError(f"OAuth code exchange failed (HTTP {exc.code}); restart login") from None
    except (urllib.error.URLError, ValueError) as exc:
        raise auth.AuthError(f"OAuth exchange unavailable ({type(exc).__name__})") from None
    if not isinstance(payload, dict):
        raise auth.AuthError("Unexpected OAuth token response")
    return payload


def validated_record(payload: dict, client_id: str, nonce: str, host: str) -> dict:
    if not all(isinstance(payload.get(x), str) and payload[x] for x in
               ("id_token", "access_token")):
        raise auth.AuthError("OAuth token response missing required credentials or scopes")
    if not isinstance(payload.get("scope", ""), str):
        raise auth.AuthError("OAuth scope response is invalid")
    scopes = payload.get("scope", "").split()
    if auth.PLAN_SCOPE in scopes and ("resource.invoke" not in scopes or not payload.get("refresh_token")):
        raise auth.AuthError("ChatGPT plan scope needs resource permission and a renewable grant")
    if payload.get("token_type", "Bearer").lower() != "bearer":
        raise auth.AuthError("Unsupported OAuth token type")
    if not isinstance(payload.get("expires_in"), (int, float)) or payload["expires_in"] <= 0:
        raise auth.AuthError("Invalid OAuth token lifetime")
    claims = validate_id_token(payload["id_token"], client_id, nonce)
    return {
        "email": claims.get("email", ""), "issuer": "https://auth.openai.com",
        "subject": claims["sub"], "client_id": client_id, "ext_agent_host_id": host,
        "id_token": payload["id_token"], "access_token": payload["access_token"],
        "refresh_token": payload.get("refresh_token", ""), "token_type": "Bearer",
        "expires_in": payload["expires_in"], "scopes": scopes,
        "saved_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    }


class Callback(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass  # Never log codes, query strings or identity tokens.

    def do_GET(self):
        parsed = urllib.parse.urlsplit(self.path)
        if parsed.path != CALLBACK_PATH:
            self.send_error(404)
            return
        self.server.result = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.end_headers()
        self.wfile.write(b"Authorization received. Return to your terminal.\n")


def run(*, new_account: bool = False, timeout: int = 180, headless: bool = False) -> None:
    host = host_id()
    try:
        previous = auth.read_saved()
    except auth.AuthError as exc:
        if "not connected" not in str(exc):
            raise
        previous = None
    if new_account:
        previous = None
    client_id = previous.get("client_id") if previous else None
    if client_id == "dynamic_agent_client":
        raise auth.AuthError("Saved OAuth registration has no issued client ID")
    state, nonce, verifier = secrets.token_urlsafe(32), secrets.token_urlsafe(32), secrets.token_urlsafe(48)
    server = http.server.HTTPServer((CALLBACK_HOST, 0), Callback)
    server.timeout = min(timeout, 600)
    try:
        redirect = f"http://{CALLBACK_HOST}:{server.server_port}{CALLBACK_PATH}"
        params = authorize_params(client_id, redirect, host, state, nonce, verifier, previous)
        url = AUTHORIZE + "?" + urllib.parse.urlencode(params)
        if headless:
            # The caller is the authenticated Amplio UI: it opens the URL on the
            # operator's machine. Never emit a token or authorization code here.
            print(json.dumps({"authorization_url": url}), flush=True)
        else:
            if not webbrowser.open(url):
                raise auth.AuthError("Could not launch system browser; set BROWSER or use a desktop session")
            print("Finish ChatGPT authorization in the browser. Waiting for callback ...")
        server.handle_request()
        answer = getattr(server, "result", None)
        if not answer:
            raise auth.AuthError("No OAuth callback; run login again")
        if answer.get("state", [None]) != [state]:
            raise auth.AuthError("OAuth state mismatch; authorization ignored")
        if answer.get("error"):
            raise auth.AuthError("OAuth authorization was declined; no credentials changed")
        code = (answer.get("code") or [""])[0]
        if not code:
            raise auth.AuthError("OAuth callback missing code")
        returned_id = (answer.get("client_id") or [None])[0]
        if client_id:
            if returned_id and returned_id != client_id:
                raise auth.AuthError("Returning OAuth client ID mismatch")
        elif not returned_id or not returned_id.startswith("oaiapp_"):
            raise auth.AuthError("Dynamic registration did not return an issued client ID")
        real_client = client_id or returned_id
        payload = exchange_code(code, real_client, verifier, redirect)
        record = validated_record(payload, real_client, nonce, host)
        # Returning account identity must not silently switch during reauthorization.
        if previous and (previous.get("subject") != record["subject"] or
                         previous.get("client_id") != record["client_id"]):
            raise auth.AuthError("Returning ChatGPT account identity mismatch")
        auth.save_validated(record, expected_subject=previous["subject"] if previous else None)
        if auth.PLAN_SCOPE in record["scopes"]:
            print("ChatGPT plan connected to Amplio. Credentials stored locally with owner-only permissions.")
        else:
            print("ChatGPT identity connected; plan access was not granted. Run login again to request consent.")
    finally:
        server.server_close()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--new-account", action="store_true", help="register a different ChatGPT account")
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--headless", action="store_true", help="print authorization URL as JSON for Amplio UI")
    args = parser.parse_args()
    try:
        run(new_account=args.new_account, timeout=args.timeout, headless=args.headless)
    except (auth.AuthError, OSError) as exc:
        print(f"ChatGPT sign-in failed: {exc}", file=sys.stderr)
        raise SystemExit(1) from None


if __name__ == "__main__":
    main()
