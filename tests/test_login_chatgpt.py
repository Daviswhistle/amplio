"""Official dynamic SIWC OAuth request and OIDC validation contracts."""
from __future__ import annotations

import importlib.util
import json
import os
import sys
import tempfile
import unittest
import urllib.parse
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "bridges"))
import siwc_auth as auth

script = ROOT / "scripts" / "login_chatgpt.py"
spec = importlib.util.spec_from_file_location("amplio_login_chatgpt", script)
login = importlib.util.module_from_spec(spec)
spec.loader.exec_module(login)


class LoginTests(unittest.TestCase):
    def test_dynamic_registration_has_exact_documented_scopes_and_pkce(self):
        p = login.authorize_params(None, "http://127.0.0.1:5000/auth/callback", "urn:uuid:test",
                                   "state", "nonce", "verifier", None)
        self.assertEqual(p["client_id"], "dynamic_agent_client")
        self.assertEqual(p["agent_name_hint"], "Amplio")
        self.assertEqual(p["resource"], "https://api.openai.com/v1")
        self.assertEqual(p["ext_agent_host_id"], "urn:uuid:test")
        self.assertEqual(p["code_challenge_method"], "S256")
        self.assertEqual(p["scope"].split(), login.SCOPES.split())
        self.assertEqual(p["redirect_uri"], "http://127.0.0.1:5000/auth/callback")

    def test_returning_auth_uses_issued_client_not_dynamic(self):
        p = login.authorize_params("oaiapp_1", "http://127.0.0.1:1000/auth/callback",
                                   "host", "state", "nonce", "verifier", {"id_token": "id-hint"})
        self.assertEqual(p["client_id"], "oaiapp_1")
        self.assertEqual(p["id_token_hint"], "id-hint")
        self.assertNotIn("agent_name_hint", p)

    def test_missing_plan_scope_must_not_be_saved(self):
        p = {"id_token": "id", "access_token": "access", "refresh_token": "refresh",
             "expires_in": 3600, "scope": "openid profile email offline_access resource.invoke"}
        with mock.patch.object(login, "validate_id_token", return_value={"sub": "user-1"}):
            result = login.validated_record(p, "oaiapp_1", "nonce", "host")
        self.assertNotIn(auth.PLAN_SCOPE, result["scopes"])
        reauthorize = login.authorize_params("oaiapp_1", "http://127.0.0.1:5000/auth/callback",
                                         "host", "state", "nonce", "verifier", result)
        self.assertEqual(reauthorize["prompt"], "consent")

    def test_oidc_payload_validated_before_record(self):
        p = {"id_token": "id", "access_token": "access", "refresh_token": "refresh",
             "expires_in": 3600, "scope": login.SCOPES}
        with mock.patch.object(login, "validate_id_token", return_value={"sub": "user-1", "email": "foo@example.com"}):
            data = login.validated_record(p, "oaiapp_1", "nonce", "host")
        self.assertEqual(data["client_id"], "oaiapp_1")
        self.assertEqual(data["subject"], "user-1")
        self.assertEqual(data["scopes"], login.SCOPES.split())

    def test_oidc_rejects_wrong_audience_nonce_and_signature(self):
        try:
            import jwt
            from cryptography.hazmat.primitives.asymmetric import rsa
        except ImportError:
            self.skipTest("PyJWT[crypto] only required for interactive login")
        import datetime
        now = datetime.datetime.now(datetime.timezone.utc)
        private = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        key = type("TestKey", (), {"key": private.public_key()})()
        claims = {"sub": "user-1", "iss": "https://auth.openai.com", "aud": "oaiapp_test",
                  "exp": now + datetime.timedelta(hours=1), "iat": now, "nonce": "expected"}
        with mock.patch.object(jwt.PyJWKClient, "get_signing_key_from_jwt", return_value=key):
            token = jwt.encode(claims, private, algorithm="RS256")
            self.assertEqual(login.validate_id_token(token, "oaiapp_test", "expected")["sub"], "user-1")
            for requested_client, supplied_nonce in [("oaiapp_other", "expected"),
                                                     ("oaiapp_test", "wrong")]:
                with self.assertRaisesRegex(auth.AuthError, "ID token"):
                    login.validate_id_token(token, requested_client, supplied_nonce)
            other = rsa.generate_private_key(public_exponent=65537, key_size=2048)
            tampered = jwt.encode(claims, other, algorithm="RS256")
            with self.assertRaisesRegex(auth.AuthError, "ID token"):
                login.validate_id_token(tampered, "oaiapp_test", "expected")


if __name__ == "__main__":
    unittest.main()
