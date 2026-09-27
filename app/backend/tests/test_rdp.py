import uuid

import pytest
from fastapi.testclient import TestClient
from sqlalchemy import select
from starlette.websockets import WebSocketDisconnect
from test_api import BROKER, ORIGIN, as_user, write_headers

from jump.db import get_db
from jump.main import app, expire_pending_rdp, finish_remote_session
from jump.models import (
    AgentIdentity,
    AgentUpdate,
    AuditEvent,
    Credential,
    Device,
    RemoteSession,
    Role,
    now,
)
from jump.rdp import instruction, parse_instruction


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url=ORIGIN) as client:
        yield client
    app.dependency_overrides.clear()


def seed(db, os_family="windows", capabilities=None):
    device = Device(
        hostname="server",
        os_family=os_family,
        architecture="amd64",
        agent_version="v0.1.0",
        capabilities=capabilities if capabilities is not None else ["rdp", "rdp_tunnel_v1"],
        addresses=[],
        online=True,
        connection_id=str(uuid.uuid4()),
    )
    db.add(device)
    db.flush()
    db.add(AgentIdentity(device_id=device.id, public_key=uuid.uuid4().bytes * 2))
    db.commit()
    return device


def credential(client, device):
    response = client.post(
        f"/api/devices/{device.id}/credentials",
        headers=write_headers(),
        json={
            "label": "Admin",
            "kind": "windows_password",
            "username": "admin",
            "domain": "EXAMPLE",
            "secret": "do-not-expose-me",
        },
    )
    assert response.status_code == 201, response.text
    assert "do-not-expose-me" not in response.text
    return response.json()["id"]


def create(client, device, cred):
    return client.post(
        f"/api/devices/{device.id}/rdp-sessions",
        headers=write_headers(),
        json={"credential_id": cred, "width": 1280, "height": 720, "dpi": 96},
    )


def test_rdp_authorization_capability_credential_and_lifecycle(client, db):
    device = seed(db)
    assert client.get(f"/api/devices/{device.id}/credentials").status_code == 401
    assert (
        client.post(
            f"/api/devices/{device.id}/rdp-sessions", headers=write_headers(), json={}
        ).status_code
        == 403
    )
    user = as_user(client, db, Role.USER)
    assert create(client, device, str(uuid.uuid4())).status_code == 403
    user.role = Role.ADMIN
    db.commit()
    assert client.post(f"/api/devices/{device.id}/rdp-sessions", json={}).status_code == 403
    cred = credential(client, device)
    stored = db.get(Credential, uuid.UUID(cred))
    assert stored.domain == "EXAMPLE" and b"do-not-expose-me" not in stored.ciphertext
    assert "do-not-expose-me" not in client.get(f"/api/devices/{device.id}/credentials").text
    other = seed(db)
    assert create(client, other, cred).status_code == 400
    linux = seed(db, "linux")
    assert create(client, linux, cred).status_code == 409
    device.capabilities = ["rdp"]
    db.commit()
    denied = create(client, device, cred)
    assert (
        denied.status_code == 409
        and denied.json()["detail"] == "Update the Jump agent to enable browser RDP."
    )
    assert db.scalar(select(RemoteSession)) is None
    device.capabilities = ["rdp", "rdp_tunnel_v1"]
    device.online = False
    db.commit()
    assert create(client, device, cred).status_code == 409
    device.online = True
    device.agent_identity.revoked_at = device.enrolled_at
    db.commit()
    assert create(client, device, cred).status_code == 409
    device.agent_identity.revoked_at = None
    wrong = Credential(
        device_id=device.id,
        label="key",
        kind="linux_ssh_key",
        username="admin",
        ciphertext=b"x",
        nonce=b"0" * 12,
    )
    db.add(wrong)
    db.commit()
    assert create(client, device, str(wrong.id)).status_code == 400
    response = create(client, device, cred)
    assert response.status_code == 201, response.text
    assert "do-not-expose-me" not in response.text
    session = db.get(RemoteSession, uuid.UUID(response.json()["id"]))
    assert session.protocol == "rdp" and session.user_id == user.id and session.dpi == 96
    session.state = "active"
    db.commit()
    finish_remote_session(db, session, "browser_disconnected")
    assert session.state == "closed" and session.closed_at
    assert db.scalar(select(AuditEvent).where(AuditEvent.event_type == "rdp_session_ended"))


def test_old_agent_cannot_attach_even_after_session_creation(client, db, monkeypatch):
    device = seed(db)
    as_user(client, db)
    cred = credential(client, device)
    sid = create(client, device, cred).json()["id"]
    device.capabilities = ["rdp"]
    db.commit()

    async def forbidden(*args, **kwargs):
        pytest.fail("Old agent reached RDP tunnel")

    monkeypatch.setattr("jump.main.rdp_gateway", forbidden)
    with client.websocket_connect(
        f"/ws/rdp-sessions/{sid}",
        headers={"Host": "localhost", "Origin": ORIGIN},
        subprotocols=["guacamole"],
    ) as ws:
        assert ws.accepted_subprotocol == "guacamole"
        assert "Update the Jump agent" in ws.receive_text()
    assert db.get(RemoteSession, uuid.UUID(sid)).state == "failed"


def test_ownership_update_conflict_and_guac_instruction_validation(client, db):
    device = seed(db)
    user = as_user(client, db)
    cred = credential(client, device)
    sid = create(client, device, cred).json()["id"]
    with pytest.raises(WebSocketDisconnect):
        with client.websocket_connect(
            f"/ws/rdp-sessions/{sid}",
            headers={"Host": "localhost", "Origin": "https://evil.example"},
            subprotocols=["guacamole"],
        ):
            pass
    assert db.get(RemoteSession, uuid.UUID(sid)).attached_at is None
    user.role = Role.USER
    db.commit()
    with pytest.raises(WebSocketDisconnect):
        with client.websocket_connect(
            f"/ws/rdp-sessions/{sid}",
            headers={"Host": "localhost", "Origin": ORIGIN},
            subprotocols=["guacamole"],
        ):
            pass
    assert db.get(RemoteSession, uuid.UUID(sid)).attached_at is None
    user.role = Role.ADMIN
    db.commit()
    other = as_user(client, db)
    with pytest.raises(WebSocketDisconnect):
        with client.websocket_connect(
            f"/ws/rdp-sessions/{sid}",
            headers={"Host": "localhost", "Origin": ORIGIN},
            subprotocols=["guacamole"],
        ):
            pass
    assert other.id != user.id
    as_user(client, db)
    operation = AgentUpdate(
        device_id=device.id,
        from_version="v0.1.0",
        target_version="v0.1.1",
        state="downloading",
        connection_id=device.connection_id,
    )
    db.add(operation)
    db.commit()
    assert create(client, device, cred).status_code == 409
    assert parse_instruction(instruction("select", "rdp").decode()) == ["select", "rdp"]
    with pytest.raises(ValueError):
        parse_instruction("4.file,1.x;4.key,1.1;")


def test_attached_rdp_audits_start_and_end_without_secret(client, db, monkeypatch):
    device = seed(db)
    as_user(client, db)
    cred = credential(client, device)
    sid = create(client, device, cred).json()["id"]

    async def gateway(
        ws,
        session_id,
        device_id,
        connection_id,
        user_id,
        width,
        height,
        dpi,
        username,
        domain,
        password,
        on_ready,
        on_activity,
    ):
        assert (session_id, device_id, connection_id) == (sid, str(device.id), device.connection_id)
        assert user_id == str(db.get(RemoteSession, uuid.UUID(sid)).user_id)
        assert (width, height, dpi, username, domain, password) == (
            1280,
            720,
            96,
            "admin",
            "EXAMPLE",
            b"do-not-expose-me",
        )
        on_ready()
        on_activity()
        await ws.send_text(instruction("ready", "opaque-guacd-id").decode())
        return "session_closed"

    monkeypatch.setattr("jump.main.rdp_gateway", gateway)
    with client.websocket_connect(
        f"/ws/rdp-sessions/{sid}",
        headers={"Host": "localhost", "Origin": ORIGIN},
        subprotocols=["other", "guacamole"],
    ) as ws:
        assert ws.accepted_subprotocol == "guacamole"
        assert parse_instruction(ws.receive_text())[0] == "ready"
        assert parse_instruction(ws.receive_text())[0] == "disconnect"
    session = db.get(RemoteSession, uuid.UUID(sid))
    assert session.state == "closed" and session.connected_at and session.closed_at
    events = list(db.scalars(select(AuditEvent).where(AuditEvent.event_type.like("rdp_session_%"))))
    assert [event.event_type for event in events] == ["rdp_session_started", "rdp_session_ended"]
    assert all("do-not-expose-me" not in str(event.detail) for event in events)


def test_rdp_websocket_requires_guacamole_subprotocol_before_claim(client, db):
    device = seed(db)
    as_user(client, db)
    cred = credential(client, device)
    sid = create(client, device, cred).json()["id"]
    for offered in ([], ["other"]):
        with pytest.raises(WebSocketDisconnect) as rejected:
            with client.websocket_connect(
                f"/ws/rdp-sessions/{sid}",
                headers={"Host": "localhost", "Origin": ORIGIN},
                subprotocols=offered,
            ):
                pass
        assert rejected.value.code == 1008
        assert db.get(RemoteSession, uuid.UUID(sid)).attached_at is None


def test_agent_update_blocked_while_rdp_active(client, db, monkeypatch):
    from jump.main import cfg

    monkeypatch.setattr(cfg, "jump_agent_version", "v0.1.1")
    device = seed(db, capabilities=["rdp_tunnel_v1", "agent_update_v1"])
    as_user(client, db)
    cred = credential(client, device)
    sid = create(client, device, cred).json()["id"]
    session = db.get(RemoteSession, uuid.UUID(sid))
    session.state = "active"
    db.commit()
    response = client.post(f"/api/devices/{device.id}/agent-update", headers=write_headers())
    assert response.status_code == 409 and "Close active sessions" in response.text


def test_private_stream_authorization_binds_session_owner_device_and_agent(client, db):
    device = seed(db)
    user = as_user(client, db)
    cred = credential(client, device)
    sid = create(client, device, cred).json()["id"]
    path = f"/api/internal/rdp-streams/{sid}/authorize"
    params = {
        "device_id": str(device.id),
        "user_id": str(user.id),
        "connection_id": device.connection_id,
    }
    assert client.get(path, params=params).status_code == 401
    assert client.get(path, params=params, headers=BROKER).status_code == 403
    session = db.get(RemoteSession, uuid.UUID(sid))
    session.attached_at = now()
    db.commit()
    assert client.get(path, params=params, headers=BROKER).status_code == 200
    assert (
        client.get(
            path, params={**params, "user_id": str(uuid.uuid4())}, headers=BROKER
        ).status_code
        == 403
    )
    assert (
        client.get(
            path, params={**params, "connection_id": str(uuid.uuid4())}, headers=BROKER
        ).status_code
        == 403
    )


def test_unattached_rdp_session_expires_before_update_conflict(client, db):
    from datetime import timedelta

    device = seed(db)
    as_user(client, db)
    cred = credential(client, device)
    sid = create(client, device, cred).json()["id"]
    session = db.get(RemoteSession, uuid.UUID(sid))
    session.created_at = now() - timedelta(minutes=2)
    db.commit()
    expire_pending_rdp(db, device.id)
    assert session.state == "failed" and session.failure_reason == "session_expired"
    assert db.scalar(select(AuditEvent).where(AuditEvent.event_type == "rdp_session_failed"))
