"""Offline SIWC credential contracts: no actual browser or OpenAI token needed."""
from __future__ import annotations

import datetime
import io
import json
import os
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "bridges"))
import siwc_auth as auth


def record(*, scope=True, expires=3600, access="old"):
    return {
        "issuer": "https://auth.openai.com", "subject": "user-1", "client_id": "oaiapp_assigned",
        "ext_agent_host_id": "urn:uuid:0000", "access_token": access,
        "refresh_token": "old-refresh", "id_token": "validated-oidc-id-token",
        "scopes": [auth.PLAN_SCOPE, "resource.invoke"] if scope else ["openid"],
        "saved_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "expires_in": expires,
    }


class CredentialsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        env = mock.patch.dict(os.environ, {"AMPLIO_SIWC_HOME": self.temp.name})
        env.start()
        self.addCleanup(env.stop)

    def test_save_access_and_permissions(self):
        auth.save_validated(record())
        self.assertEqual(auth.get_access_token(), "old")
        self.assertEqual(auth.credential_path().stat().st_mode & 0o777, 0o600)
        self.assertEqual(auth.home().stat().st_mode & 0o777, 0o700)
        auth.save_validated(record(scope=False))
        with self.assertRaisesRegex(auth.AuthError, "permission not granted"):
            auth.get_access_token()
        self.assertNotIn(auth.PLAN_SCOPE, auth.read_saved()["scopes"])

    def test_no_issued_client_or_identity_change(self):
        auth.save_validated(record())
        replacement = record()
        replacement["subject"] = "other-user"
        with self.assertRaisesRegex(auth.AuthError, "changed"):
            auth.save_validated(replacement, expected_subject="user-1")
        self.assertEqual(auth.read_saved()["subject"], "user-1")

    def test_cross_process_lock_friendly_refresh(self):
        auth.save_validated(record(expires=5))
        refreshed = {"access_token": "updated", "refresh_token": "new-refresh", "expires_in": 3600}
        def fake_urlopen(req, timeout):
            data = __import__("urllib.parse", fromlist=["parse_qs"]).parse_qs(req.data.decode())
            self.assertEqual(data["client_id"], ["oaiapp_assigned"])
            self.assertEqual(data["resource"], [auth.RESOURCE])
            self.assertEqual(data["refresh_token"], ["old-refresh"])
            return io.BytesIO(json.dumps(refreshed).encode())
        with mock.patch.object(auth.urllib.request, "urlopen", side_effect=fake_urlopen) as backend:
            results = []
            errors = []
            def worker():
                try:
                    results.append(auth.get_access_token())
                except Exception as exc:
                    errors.append(exc)
            threads = [threading.Thread(target=worker) for _ in range(8)]
            for t in threads:
                t.start()
            for t in threads:
                t.join()
        self.assertFalse(errors)
        self.assertEqual(results, ["updated"] * 8)
        backend.assert_called_once()
        self.assertEqual(auth.read_saved()["refresh_token"], "new-refresh")

    def test_force_refresh_avoids_second_rotation_after_first_worker(self):
        auth.save_validated(record())
        updated = {**record(), "access_token": "new", "refresh_token": "new-refresh"}
        with mock.patch.object(auth, "_update_refresh", return_value=updated) as refresh:
            # update_refresh must persist; mimic its atomic update.
            def save_and_return(_):
                auth._write(auth.credential_path(), updated)
                return updated
            refresh.side_effect = save_and_return
            self.assertEqual(auth.get_access_token(force_refresh=True, previous_token="old"), "new")
            self.assertEqual(auth.get_access_token(force_refresh=True, previous_token="old"), "new")
            refresh.assert_called_once()

    def test_terminal_invalid_grant_clears_tokens_but_preserves_registration(self):
        auth.save_validated(record(expires=5))
        error = urllib.error.HTTPError(auth.TOKEN_ENDPOINT, 400, "Bad Request", {},
                                      io.BytesIO(b'{"error":"invalid_grant"}'))
        with mock.patch.object(auth.urllib.request, "urlopen", side_effect=error):
            with self.assertRaisesRegex(auth.AuthError, "revoked or expired"):
                auth.get_access_token()
        saved = auth.read_saved()
        self.assertEqual(saved["client_id"], "oaiapp_assigned")
        self.assertNotIn("refresh_token", saved)

    def test_revoked_plan_scope_is_persisted_and_future_calls_fail_fast(self):
        auth.save_validated(record(expires=5))
        payload = {"access_token": "new", "refresh_token": "rotated", "expires_in": 3600,
                   "scope": "openid profile email"}
        with mock.patch.object(auth.urllib.request, "urlopen", return_value=io.BytesIO(json.dumps(payload).encode())):
            with self.assertRaisesRegex(auth.AuthError, "permission removed"):
                auth.get_access_token()
        self.assertNotIn(auth.PLAN_SCOPE, auth.read_saved()["scopes"])
        with self.assertRaisesRegex(auth.AuthError, "permission not granted"):
            auth.get_access_token()

    def test_network_failure_preserves_token(self):
        auth.save_validated(record(expires=5))
        error = urllib.error.URLError("offline")
        with mock.patch.object(auth.urllib.request, "urlopen", side_effect=error):
            with self.assertRaisesRegex(auth.AuthError, "unavailable"):
                auth.get_access_token()
        self.assertEqual(auth.read_saved()["refresh_token"], "old-refresh")

    def test_token_path_symlink_and_group_read_are_rejected(self):
        auth.save_validated(record())
        path = auth.credential_path()
        path.chmod(0o644)
        with self.assertRaisesRegex(auth.AuthError, "owner-only"):
            auth.read_saved()
        path.chmod(0o600)
        path.rename(path.with_suffix(".bak"))
        path.symlink_to(path.with_suffix(".bak"))
        with self.assertRaisesRegex(auth.AuthError, "symlink"):
            auth.read_saved()


if __name__ == "__main__":
    unittest.main()
