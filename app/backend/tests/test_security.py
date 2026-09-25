import hashlib
import uuid
from datetime import timedelta

import pytest
from cryptography.exceptions import InvalidTag
from fastapi import HTTPException
from sqlalchemy import select

from jump.credentials import create_credential, decrypt_for_gateway
from jump.models import AuditEvent, Device, Group, Role, Tag, now
from jump.security import (
    consume_enrollment,
    create_enrollment,
    decrypt_secret,
    encrypt_secret,
    hash_token,
    map_oidc_user,
    require_admin,
)


def test_oidc_mapping_and_roles(db):
    first = map_oidc_user(db, "issuer", {"sub": "subject-a", "email": "a@example.com"})
    second = map_oidc_user(db, "issuer", {"sub": "subject-b", "email": "b@example.com"})
    again = map_oidc_user(db, "issuer", {"sub": "subject-a", "name": "Updated"})
    assert first.role == Role.ADMIN
    assert second.role == Role.USER
    assert again.id == first.id and again.display_name == "Updated"
    assert (
        len(db.scalars(select(AuditEvent).where(AuditEvent.event_type == "user_login")).all()) == 3
    )
    with pytest.raises(HTTPException) as exc:
        require_admin(second)
    assert exc.value.status_code == 403


def test_token_hash_single_use_expiry_revocation(db):
    user = map_oidc_user(db, "issuer", {"sub": "admin"})
    token, record = create_enrollment(db, user, "linux")
    assert token != record.token_hash
    assert record.token_hash == hashlib.sha256(token.encode()).hexdigest()
    assert hash_token(token) == record.token_hash
    assert token not in repr(record)
    assert consume_enrollment(db, token, "linux").id == record.id
    with pytest.raises(HTTPException):
        consume_enrollment(db, token, "linux")
    expired, entry = create_enrollment(db, user, "linux")
    entry.expires_at = now() - timedelta(seconds=1)
    db.commit()
    with pytest.raises(HTTPException):
        consume_enrollment(db, expired, "linux")
    revoked, entry = create_enrollment(db, user, "linux")
    entry.revoked_at = now()
    db.commit()
    with pytest.raises(HTTPException):
        consume_enrollment(db, revoked, "linux")


def test_credential_authentication_and_key_isolation():
    credential_id = str(uuid.uuid4())
    ciphertext, nonce, version = encrypt_secret(b"top secret", credential_id, b"a" * 32)
    assert version == 1 and b"top secret" not in ciphertext
    assert decrypt_secret(ciphertext, nonce, credential_id, b"a" * 32) == b"top secret"
    with pytest.raises(InvalidTag):
        decrypt_secret(ciphertext, nonce, credential_id, b"b" * 32)
    with pytest.raises(InvalidTag):
        decrypt_secret(ciphertext, nonce, str(uuid.uuid4()), b"a" * 32)


def test_device_group_tags(db):
    group = Group(name="Lab")
    tag = Tag(name="Linux")
    device = Device(hostname="server-1", os_family="linux", capabilities=[], addresses=[])
    device.group = group
    device.tags.append(tag)
    db.add(device)
    db.commit()
    found = db.get(Device, device.id)
    assert found.group.name == "Lab"
    assert [t.name for t in found.tags] == ["Linux"]


def test_credential_service_never_stores_plaintext(db):
    admin = map_oidc_user(db, "issuer", {"sub": "admin"})
    device = Device(hostname="server", os_family="linux", capabilities=[], addresses=[])
    db.add(device)
    db.commit()
    item = create_credential(
        db,
        admin,
        device,
        label="Emergency",
        kind="linux_ssh_key",
        username="root",
        secret=b"private-key-data",
    )
    assert b"private-key-data" not in item.ciphertext
    assert decrypt_for_gateway(item) == b"private-key-data"
    assert db.query(AuditEvent).filter_by(event_type="credential_created").count() == 1
