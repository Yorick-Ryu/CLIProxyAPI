"""Keep untrusted browser and legacy text outside the public event contract."""
import base64
import json
import os
import re
import stat
from pathlib import Path
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

EMAIL = re.compile(r"[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
IDENTIFIER = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}\Z")
REASONS = frozenset('success failed queued running human_required cancelled expired expiry_unknown workspace_mismatch configuration_changed missing_credentials ambiguous_account disabled free healthy transient unknown locked retry_wait attempts_exhausted notification_pending unavailable legacy no_history recorded quality_passed quality_failed user_skipped priority_unchanged ledger_pending reconciliation_required lease_lost draft_expired invalid_input conflict'.split())


class OpsError(Exception):
    def __init__(self, reason, status=400):
        self.reason = reason if reason in REASONS else 'failed'
        self.status = status
        super().__init__(self.reason)


def email(value):
    value = str(value).strip()
    if not EMAIL.fullmatch(value) or len(value) > 254:
        raise OpsError('invalid_input')
    local, domain = value.rsplit('@', 1)
    return local + '@' + domain.lower()


def lock_email(value):
    # Keep the displayed local part and aliases; case-fold only the lock identity.
    return email(value).casefold()


def totp(value):
    value = str(value).replace(' ', '').upper().rstrip('=')
    if not re.fullmatch('[A-Z2-7]{16,128}', value):
        raise OpsError('invalid_input')
    try:
        decoded = base64.b32decode(value + '=' * (-len(value) % 8))
    except ValueError:
        raise OpsError('invalid_input') from None
    if len(decoded) < 10:
        raise OpsError('invalid_input')
    return value


def private_file(path):
    path = Path(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
        raise OpsError('unavailable', 503)
    return path.read_bytes()


class Vault:
    def __init__(self, key_path):
        key = private_file(key_path)
        if len(key) != 32:
            raise OpsError('unavailable', 503)
        self.cipher = AESGCM(key)

    def seal(self, value, purpose):
        nonce = os.urandom(12)
        return nonce + self.cipher.encrypt(nonce, json.dumps(value, ensure_ascii=False).encode(), purpose.encode())

    def open(self, value, purpose):
        return json.loads(self.cipher.decrypt(value[:12], value[12:], purpose.encode()))
