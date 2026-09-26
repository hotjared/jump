import base64
import json
import uuid

import pytest
from fastapi.testclient import TestClient
from sqlalchemy import select
from starlette.websockets import WebSocketDisconnect
from test_api import ORIGIN, as_user, write_headers

from jump.db import get_db
from jump.main import app, finish_ssh_session, trust_ssh_host_key
from jump.models import AgentIdentity, AuditEvent, Credential, Device, RemoteSession, Role, User


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url=ORIGIN) as client:
        yield client
    app.dependency_overrides.clear()


def seeded(db):
    device = Device(
        hostname="server",
        os_family="linux",
        os_version="Ubuntu",
        architecture="amd64",
        agent_version="test",
        capabilities=["ssh"],
        addresses=[],
        online=True,
        connection_id=str(uuid.uuid4()),
    )
    db.add(device)
    db.flush()
    db.add(AgentIdentity(device_id=device.id, public_key=uuid.uuid4().bytes * 2))
    db.commit()
    return device


def credential(client, db, device):
    response = client.post(
        f"/api/devices/{device.id}/credentials",
        headers=write_headers(),
        json={
            "label": "Root",
            "kind": "linux_password",
            "username": "root",
            "secret": "super-private-password",
        },
    )
    assert response.status_code == 201, response.text
    assert "super-private-password" not in response.text
    assert (
        b"super-private-password"
        not in db.get(Credential, uuid.UUID(response.json()["id"])).ciphertext
    )
    return response.json()["id"]


def create(client, device, cred):
    return client.post(
        f"/api/devices/{device.id}/ssh-sessions",
        headers=write_headers(),
        json={"credential_id": cred, "columns": 80, "rows": 24},
    )


def test_session_authorization_validation_and_host_trust(client, db):
    device = seeded(db)
    assert client.get(f"/api/devices/{device.id}/credentials").status_code == 401
    user = as_user(client, db)
    cred = credential(client, db, device)
    other = seeded(db)
    assert create(client, other, cred).status_code == 400
    device.online = False
    db.commit()
    assert create(client, device, cred).status_code == 409
    device.online = True
    device.agent_identity.revoked_at = device.enrolled_at
    db.commit()
    assert create(client, device, cred).status_code == 409
    device.agent_identity.revoked_at = None
    device.capabilities = []
    db.commit()
    assert create(client, device, cred).status_code == 409
    device.capabilities = ["ssh"]
    db.commit()
    response = create(client, device, cred)
    assert response.status_code == 201, response.text
    assert "super-private-password" not in response.text
    session = db.get(RemoteSession, uuid.UUID(response.json()["id"]))
    assert session.user_id == user.id and session.credential_id == uuid.UUID(cred)
    assert trust_ssh_host_key(db, session, "SHA256:first")
    assert device.ssh_host_key == "SHA256:first"
    assert trust_ssh_host_key(db, session, "SHA256:first")
    assert not trust_ssh_host_key(db, session, "SHA256:changed")
    assert device.ssh_host_key == "SHA256:first"
    session.state = "active"
    finish_ssh_session(db, session, "idle_timeout")
    assert session.state == "closed" and session.closed_at
    events = [e.event_type for e in db.scalars(select(AuditEvent))]
    assert events.count("ssh_host_key_trusted") == 1
    assert "ssh_session_ended" in events


def test_reset_is_admin_only_and_requires_csrf(client, db):
    device = seeded(db)
    device.ssh_host_key = "SHA256:old"
    db.commit()
    user = as_user(client, db, Role.USER)
    path = f"/api/devices/{device.id}/ssh-host-key/reset"
    assert client.post(path, headers=write_headers()).status_code == 403
    user.role = Role.ADMIN
    db.commit()
    assert client.post(path).status_code == 403
    assert client.post(path, headers=write_headers()).status_code == 200
    assert device.ssh_host_key is None
    assert db.scalar(select(AuditEvent).where(AuditEvent.event_type == "ssh_host_key_reset"))


def test_session_attach_requires_ownership_and_same_origin(client, db):
    device = seeded(db)
    first = as_user(client, db)
    cred = credential(client, db, device)
    sid = create(client, device, cred).json()["id"]
    with pytest.raises(WebSocketDisconnect):
        with client.websocket_connect(
            f"/ws/sessions/{sid}", headers={"Host": "localhost", "Origin": "https://evil.example"}
        ):
            pass
    assert db.get(RemoteSession, uuid.UUID(sid)).attached_at is None
    second = User(
        oidc_issuer="issuer",
        oidc_subject=str(uuid.uuid4()),
        email="other@example.com",
        display_name="Other",
        role=Role.USER,
    )
    db.add(second)
    db.commit()
    state = base64.b64encode(json.dumps({"uid": str(second.id), "csrf": "test-csrf"}).encode())
    from itsdangerous import TimestampSigner

    cookie = TimestampSigner("test-session-secret-at-least-32-characters").sign(state).decode()
    client.cookies.set("jump_session", cookie)
    with pytest.raises(WebSocketDisconnect):
        with client.websocket_connect(
            f"/ws/sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
        ):
            pass
    assert db.get(RemoteSession, uuid.UUID(sid)).user_id == first.id
    assert db.get(RemoteSession, uuid.UUID(sid)).attached_at is None


def test_browser_gateway_opens_only_selected_credential(client, db, monkeypatch):
    device = seeded(db)
    as_user(client, db)
    cred = credential(client, db, device)
    sid = create(client, device, cred).json()["id"]
    sent = []

    class FakeBroker:
        async def send(self, frame):
            sent.append(json.loads(frame))

        async def recv(self):
            if len(sent) == 1:
                return json.dumps(
                    {"type": "session_opened", "session_id": sid, "fingerprint": "SHA256:first"}
                )
            return json.dumps({"type": "session_close", "session_id": sid})

        async def close(self):
            pass

    async def fake_connect(*args, **kwargs):
        return FakeBroker()

    monkeypatch.setattr("jump.main.ws_connect", fake_connect)
    with client.websocket_connect(
        f"/ws/sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
    ) as ws:
        assert ws.receive_json()["state"] == "active"
        assert ws.receive_json()["state"] == "closed"
    assert sent[0]["kind"] == "linux_password"
    assert base64.b64decode(sent[0]["secret"]) == b"super-private-password"
    session = db.get(RemoteSession, uuid.UUID(sid))
    assert session.state == "closed"
    assert device.ssh_host_key == "SHA256:first"
    assert db.scalar(select(AuditEvent).where(AuditEvent.event_type == "ssh_session_started"))
