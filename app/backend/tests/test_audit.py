import base64
import json
import uuid
from datetime import timedelta

import pytest
from fastapi.testclient import TestClient
from itsdangerous import TimestampSigner

from jump.db import get_db
from jump.main import app
from jump.models import AuditEvent, Device, Role, User, now


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url="http://localhost:8000") as test_client:
        yield test_client
    app.dependency_overrides.clear()


def login(client, db, role):
    user = User(
        oidc_issuer="issuer",
        oidc_subject=str(uuid.uuid4()),
        email="jared@example.com",
        display_name="Jared Rodriguez",
        role=role,
    )
    db.add(user)
    db.commit()
    state = base64.b64encode(json.dumps({"uid": str(user.id), "csrf": "test-csrf"}).encode())
    client.cookies.set(
        "jump_session",
        TimestampSigner("test-session-secret-at-least-32-characters").sign(state).decode(),
    )
    return user


def test_audit_permissions_and_safe_context(client, db):
    assert client.get("/api/audit").status_code == 401
    actor = login(client, db, Role.USER)
    assert client.get("/api/audit").status_code == 403
    actor.role = Role.ADMIN
    device = Device(hostname="prod-dc01", display_name="PROD-DC01", os_family="windows")
    db.add(device)
    db.flush()
    old = now() - timedelta(hours=1)
    events = [
        AuditEvent(
            event_type="rdp_session_failed",
            actor_user_id=actor.id,
            device_id=device.id,
            created_at=old,
            request_id="request-123",
            detail={
                "session_id": str(uuid.uuid4()),
                "reason": "guacd_disconnected",
                "password": "password-secret",
                "credential_id": str(uuid.uuid4()),
                "ciphertext": "ciphertext-secret",
                "nonce": "nonce-secret",
                "clipboard": "clipboard-secret",
                "token": "token-secret",
            },
        ),
        AuditEvent(
            event_type="file_download_failed",
            device_id=device.id,
            detail={
                "filename": "backup.zip",
                "size": 128,
                "reason": "checksum_mismatch",
                "remote_path": "/private/backup.zip",
                "private_key": "key-secret",
            },
        ),
        AuditEvent(
            event_type="agent_update_failed",
            actor_user_id=actor.id,
            device_id=device.id,
            detail={
                "from_version": "v0.1.6",
                "target_version": "v0.1.7",
                "reason": "reconnect_timeout",
                "operation_id": str(uuid.uuid4()),
            },
        ),
        AuditEvent(
            event_type="credential_updated",
            actor_user_id=actor.id,
            detail={
                "label": "Domain Admin",
                "kind": "password",
                "password": "credential-secret",
                "ciphertext": "ciphertext-secret",
                "nonce": "nonce-secret",
            },
        ),
        AuditEvent(event_type="unknown_future_event", detail={"arbitrary": "arbitrary-secret"}),
    ]
    db.add_all(events)
    db.commit()
    response = client.get("/api/audit")
    assert response.status_code == 200
    data = response.json()
    assert data[-1]["id"] == str(events[0].id)
    session = data[-1]
    assert session["actor"] == {"name": "Jared Rodriguez", "email": "jared@example.com"}
    assert session["device"] == {"id": str(device.id), "name": "PROD-DC01"}
    assert session["detail"] == {
        "session_id": events[0].detail["session_id"],
        "reason": "guacd_disconnected",
    }
    assert session["request_id"] == "request-123"
    by_type = {e["event_type"]: e for e in data}
    assert by_type["file_download_failed"]["actor"] is None
    assert by_type["file_download_failed"]["detail"] == {
        "filename": "backup.zip",
        "size": 128,
        "reason": "checksum_mismatch",
    }
    assert by_type["agent_update_failed"]["detail"]["from_version"] == "v0.1.6"
    assert by_type["agent_update_failed"]["detail"]["target_version"] == "v0.1.7"
    assert by_type["credential_updated"]["detail"] == {"label": "Domain Admin", "kind": "password"}
    assert by_type["unknown_future_event"]["detail"] == {}
    for secret in (
        "password-secret",
        "credential-secret",
        "ciphertext-secret",
        "nonce-secret",
        "clipboard-secret",
        "token-secret",
        "key-secret",
        "arbitrary-secret",
        "/private/backup.zip",
    ):
        assert secret not in response.text


def test_audit_deleted_context_and_rejects_unstructured_reason(client, db):
    login(client, db, Role.ADMIN)
    db.add(
        AuditEvent(
            event_type="ssh_session_failed",
            detail={"reason": "Traceback: private exception", "session_id": "bad/path"},
        )
    )
    db.commit()
    data = client.get("/api/audit").json()[0]
    assert data["actor"] is None and data["device"] is None
    assert data["detail"] == {}
