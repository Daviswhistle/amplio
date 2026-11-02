"""Credential storage for OpenAI's documented open-source Sign in with ChatGPT flow.

No OpenAI tokens or OAuth authorization URLs are written to logs. The login CLI
validates OIDC identity; this module only handles the previously validated
credential record and bounded refresh, under a cross-process file lock.
"""

from __future__ import annotations

import contextlib
import datetime
import fcntl
import json
import os
import stat
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Iterator

RESOURCE = "https://api.openai.com/v1"
TOKEN_ENDPOINT = "https://auth.openai.com/api/accounts/oauth/token"
PLAN_SCOPE = "chatgpt.tokens.use.direct"
TOKEN_REFRESH_MARGIN = 300
_LOCK = threading.Lock()


class AuthError(Exception):
    """Human-readable error, with no token values or request bodies."""


def home() -> Path:
    return Path(os.environ.get("AMPLIO_SIWC_HOME", "~/.config/amplio-chatgpt/siwc")).expanduser()


def credential_path() -> Path:
    return home() / "account.json"


def _ensure_private_dir(path: Path) -> None:
    if path.is_symlink():
        raise AuthError("OAuth credential directory cannot be a symlink")
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    if path.stat().st_mode & 0o077:
        raise AuthError("OAuth credential directory must be owner-only (chmod 700)")


def _read(path: Path) -> dict[str, Any]:
    try:
        if path.is_symlink():
            raise AuthError("OAuth credentials cannot be a symlink")
        if path.stat().st_mode & 0o077:
            raise AuthError("OAuth credentials must be owner-only (chmod 600)")
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise AuthError("ChatGPT plan not connected; run python3 scripts/login_chatgpt.py") from exc
    except (ValueError, TypeError) as exc:
        raise AuthError("ChatGPT credential file is invalid; sign in again") from exc
    if not isinstance(data, dict):
        raise AuthError("ChatGPT credential record must be a JSON object")
    return data


def _write(path: Path, data: dict[str, Any]) -> None:
    _ensure_private_dir(path.parent)
    fd, tmp = tempfile.mkstemp(prefix=".siwc-", dir=path.parent)
    try:
        os.fchmod(fd, stat.S_IRUSR | stat.S_IWUSR)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(data, f, separators=(",", ":"), ensure_ascii=False)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


@contextlib.contextmanager
def _locked() -> Iterator[None]:
    """Share one lock across every Amplio bridge subprocess and login CLI."""
    _ensure_private_dir(home())
    with _LOCK, (home() / ".credentials.lock").open("a+b") as f:
        os.fchmod(f.fileno(), 0o600)
        fcntl.flock(f.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(f.fileno(), fcntl.LOCK_UN)


def save_validated(data: dict[str, Any], *, expected_subject: str | None = None) -> None:
    """Called only after the login CLI validates a signed OIDC ID token."""
    if not all(isinstance(data.get(k), str) and data[k] for k in
               ("client_id", "subject", "issuer", "access_token", "id_token")):
        raise AuthError("Refusing to persist incomplete OAuth credentials")
    if data["issuer"] != "https://auth.openai.com":
        raise AuthError("Unexpected OAuth issuer")
    if not isinstance(data.get("scopes"), list):
        raise AuthError("OAuth credential record missing granted scopes")
    if PLAN_SCOPE in data["scopes"]:
        if "resource.invoke" not in data["scopes"]:
            raise AuthError("ChatGPT plan usage requires resource.invoke permission")
        if not data.get("refresh_token"):
            raise AuthError("ChatGPT plan usage requires a renewable access token")
    with _locked():
        if expected_subject is not None:
            previous = _read(credential_path())
            if (data["subject"] != expected_subject or previous.get("subject") != expected_subject
                    or previous.get("client_id") != data["client_id"]):
                raise AuthError("OAuth account changed during sign-in; previous login retained")
        _write(credential_path(), data)


def read_saved() -> dict[str, Any]:
    with _locked():
        return _read(credential_path())


def _expiry(data: dict[str, Any]) -> float:
    """Saved at is UTC ISO8601, expiry duration is seconds per token response."""
    try:
        timestamp = datetime.datetime.fromisoformat(data["saved_at"].replace("Z", "+00:00")).timestamp()
        expires_in = int(data["expires_in"])
        if expires_in <= 0:
            raise ValueError("invalid expiry")
        return timestamp + expires_in
    except (KeyError, TypeError, ValueError, OverflowError) as exc:
        raise AuthError("OAuth credential expiry is invalid; sign in again") from exc


def _update_refresh(data: dict[str, Any]) -> dict[str, Any]:
    client_id, refresh_token = data.get("client_id"), data.get("refresh_token")
    if not client_id or client_id == "dynamic_agent_client" or not refresh_token:
        raise AuthError("Missing issued OAuth client or refresh token; sign in again")
    body = urllib.parse.urlencode({
        "grant_type": "refresh_token", "client_id": client_id,
        "refresh_token": refresh_token, "resource": RESOURCE,
    }).encode()
    request = urllib.request.Request(TOKEN_ENDPOINT, data=body, method="POST",
                                     headers={"Content-Type": "application/x-www-form-urlencoded"})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            payload = json.load(response)
    except urllib.error.HTTPError as exc:
        try:
            error = json.loads(exc.read(2048)).get("error", "")
            if isinstance(error, dict):
                error = error.get("code", "")
        except (ValueError, TypeError, AttributeError):
            error = ""
        if error in {"invalid_grant", "invalid_refresh_token", "token_expired",
                     "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused"}:
            # A confirmed terminal rejection: clear only the credentials, retaining
            # the registered client + account identity for the next sign-in.
            cleared = {k: v for k, v in data.items() if k not in
                       {"access_token", "refresh_token", "id_token", "expires_in", "saved_at"}}
            _write(credential_path(), cleared)
            raise AuthError("ChatGPT OAuth session revoked or expired; sign in again") from None
        raise AuthError(f"ChatGPT OAuth token refresh HTTP {exc.code}; no billing fallback") from None
    except (urllib.error.URLError, ValueError, TypeError) as exc:
        raise AuthError(f"ChatGPT OAuth refresh unavailable ({type(exc).__name__})") from None
    if not isinstance(payload, dict) or not payload.get("access_token"):
        raise AuthError("OAuth refresh returned no access token")
    if payload.get("token_type", "Bearer").lower() != "bearer":
        raise AuthError("OAuth refresh returned unsupported token type")
    if not isinstance(payload.get("expires_in"), (int, float)) or payload["expires_in"] <= 0:
        raise AuthError("OAuth refresh returned invalid expiry")
    updated = dict(data)
    updated.update({"access_token": payload["access_token"],
                    "refresh_token": payload.get("refresh_token") or data["refresh_token"],
                    "expires_in": payload["expires_in"],
                    "saved_at": datetime.datetime.now(datetime.timezone.utc).isoformat()})
    # Without validated OIDC identity, do not replace the original id_token hint.
    if "scope" in payload:
        if not isinstance(payload["scope"], str):
            raise AuthError("OAuth refresh returned malformed scope")
        updated["scopes"] = payload["scope"].split()
        if PLAN_SCOPE not in updated["scopes"] or "resource.invoke" not in updated["scopes"]:
            # Save the reduced scopes first, so no later subprocess can use
            # the previous grant after the server has revoked plan usage.
            _write(credential_path(), updated)
            raise AuthError("ChatGPT plan permission removed during refresh; sign in again")
    _write(credential_path(), updated)
    return updated


def get_access_token(*, force_refresh: bool = False, previous_token: str | None = None) -> str:
    """Refresh once for a shared session; avoids races across subprocesses."""
    with _locked():
        data = _read(credential_path())
        if PLAN_SCOPE not in data.get("scopes", []) or "resource.invoke" not in data.get("scopes", []):
            raise AuthError("ChatGPT plan permission not granted; sign in again")
        access = data.get("access_token")
        if not access:
            raise AuthError("ChatGPT access token unavailable; sign in again")
        if (force_refresh and (previous_token is None or previous_token == access)) or \
                _expiry(data) <= time.time() + TOKEN_REFRESH_MARGIN:
            data = _update_refresh(data)
            access = data["access_token"]
        return access
