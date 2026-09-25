import base64
import json
import uuid

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from fastapi.testclient import TestClient
from itsdangerous import TimestampSigner
from sqlalchemy import select

from jump.db import get_db
from jump.main import app
from jump.models import AgentIdentity, AuditEvent, Device, Role, User

ORIGIN = "http://localhost:8000"
BROKER = {"Authorization": "Bearer test-broker-token-at-least-32-characters"}
META = {
    "hostname": "linux-host",
    "os_family": "linux",
    "os_version": "Ubuntu",
    "architecture": "amd64",
    "agent_version": "0.1.0",
    "capabilities": ["ssh", "filesystem"],
    "addresses": ["192.0.2.4"],
}


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url=ORIGIN) as client:
        yield client
    app.dependency_overrides.clear()


def as_user(client, db, role=Role.ADMIN):
    user = User(
        oidc_issuer="issuer",
        oidc_subject=str(uuid.uuid4()),
        email="admin@example.com",
        display_name="Admin",
        role=role,
    )
    db.add(user)
    db.commit()
    state = base64.b64encode(json.dumps({"uid": str(user.id), "csrf": "test-csrf"}).encode())
    cookie = TimestampSigner("test-session-secret-at-least-32-characters").sign(state).decode()
    client.cookies.set("jump_session", cookie)
    assert client.get("/api/me").json()["csrf"] == "test-csrf"
    return user


def write_headers():
    return {"Origin": ORIGIN, "X-CSRF-Token": "test-csrf"}


def test_permissions_and_csrf(client, db):
    assert client.get("/api/devices").status_code == 401
    as_user(client, db, Role.USER)
    assert client.get("/api/devices").status_code == 200
    assert (
        client.post("/api/groups", json={"name": "Lab"}, headers=write_headers()).status_code == 403
    )
    assert client.post("/api/groups", json={"name": "Lab"}).status_code == 403
    assert client.get("/api/audit").status_code == 403


def test_enrollment_presence_reconnect_and_audit(client, db):
    as_user(client, db)
    no_csrf = client.post("/api/enrollment-tokens", json={"os_family": "linux"})
    assert no_csrf.status_code == 403
    created = client.post(
        "/api/enrollment-tokens", json={"os_family": "linux"}, headers=write_headers()
    )
    assert created.status_code == 200, created.text
    token = created.json()["token"]
    pub = Ed25519PrivateKey.generate().public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    enroll = {"token": token, "public_key": base64.b64encode(pub).decode(), "metadata": META}
    assert client.post("/api/internal/enroll", json=enroll).status_code == 401
    response = client.post("/api/internal/enroll", json=enroll, headers=BROKER)
    assert response.status_code == 200
    device_id = response.json()["device_id"]
    assert client.post("/api/internal/enroll", json=enroll, headers=BROKER).status_code == 400
    assert db.scalar(select(AgentIdentity).where(AgentIdentity.device_id == uuid.UUID(device_id)))
    assert client.get("/api/devices").json()[0]["hostname"] == "linux-host"
    url = f"/api/internal/devices/{device_id}"

    def post(suffix, cid):
        return client.post(
            url + suffix, json={"connection_id": cid, "metadata": META}, headers=BROKER
        )

    assert post("/connected", "new-connection").status_code == 200
    assert post("/heartbeat", "new-connection").status_code == 200
    assert post("/connected", "replacement").status_code == 200
    assert post("/disconnected", "new-connection").status_code == 200
    assert db.get(Device, uuid.UUID(device_id)).online
    assert post("/heartbeat", "new-connection").status_code == 409
    assert post("/heartbeat", "replacement").status_code == 200
    assert post("/disconnected", "replacement").status_code == 200
    assert not db.get(Device, uuid.UUID(device_id)).online
    events = [e.event_type for e in db.scalars(select(AuditEvent))]
    assert (
        "device_enrolled" in events
        and "agent_connected" in events
        and "agent_disconnected" in events
    )


def test_groups_tags_and_device_edit(client, db):
    as_user(client, db)
    group = client.post("/api/groups", json={"name": "Production"}, headers=write_headers()).json()
    tag = client.post("/api/tags", json={"name": "Important"}, headers=write_headers()).json()
    device = Device(hostname="host", os_family="linux", capabilities=[], addresses=[])
    db.add(device)
    db.commit()
    response = client.patch(
        f"/api/devices/{device.id}",
        headers=write_headers(),
        json={"display_name": "Web", "group_id": group["id"], "tag_ids": [tag["id"]]},
    )
    assert response.status_code == 200
    assert response.json()["group"]["name"] == "Production"
    assert response.json()["tags"][0]["name"] == "Important"
    renamed = client.put(
        f"/api/groups/{group['id']}", headers=write_headers(), json={"name": "Infrastructure"}
    )
    assert renamed.json()["name"] == "Infrastructure"
    assert client.delete(f"/api/tags/{tag['id']}", headers=write_headers()).status_code == 200
    db.expire_all()
    assert client.get("/api/devices").json()[0]["tags"] == []


def test_revoke_identity_disconnects_and_prevents_reconnect(client, db, monkeypatch):
    user = as_user(client, db)
    created = client.post(
        "/api/enrollment-tokens", json={"os_family": "linux"}, headers=write_headers()
    ).json()
    pub = Ed25519PrivateKey.generate().public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    enrollment = client.post(
        "/api/internal/enroll",
        json={
            "token": created["token"],
            "public_key": base64.b64encode(pub).decode(),
            "metadata": META,
        },
        headers=BROKER,
    )
    assert enrollment.status_code == 200
    device_id = enrollment.json()["device_id"]
    url = f"/api/internal/devices/{device_id}"
    presence = {"connection_id": "live-connection", "metadata": META}
    assert client.post(url + "/connected", json=presence, headers=BROKER).status_code == 200

    disconnected = []
    monkeypatch.setattr("jump.main.disconnect_revoked_agent", disconnected.append)
    user.role = Role.USER
    db.commit()
    assert client.post(url + "/revoke", headers=write_headers()).status_code == 403
    user.role = Role.ADMIN
    db.commit()
    assert client.post(url + "/revoke").status_code == 403
    response = client.post(url + "/revoke", headers=write_headers())
    assert response.status_code == 200, response.text
    assert response.json()["identity_state"] == "revoked"
    assert response.json()["online"] is False
    assert disconnected == [uuid.UUID(device_id)]
    assert client.get("/api/internal/identities/" + device_id, headers=BROKER).status_code == 404
    assert client.post(url + "/connected", json=presence, headers=BROKER).status_code == 403
    assert client.post(url + "/heartbeat", json=presence, headers=BROKER).status_code == 409
    assert db.get(Device, uuid.UUID(device_id)) is not None
    assert client.get(url).json()["identity_state"] == "revoked"
    assert client.post(url + "/revoke", headers=write_headers()).status_code == 200
    assert disconnected == [uuid.UUID(device_id), uuid.UUID(device_id)]
    events = db.scalars(
        select(AuditEvent).where(AuditEvent.event_type == "agent_identity_revoked")
    ).all()
    assert len(events) == 1
    assert events[0].actor_user_id == user.id
    assert events[0].device_id == uuid.UUID(device_id)


def test_failed_broker_disconnect_preserves_revocation(client, db, monkeypatch):
    from fastapi import HTTPException

    as_user(client, db)
    device = Device(hostname="host", os_family="linux", capabilities=[], addresses=[])
    db.add(device)
    db.flush()
    db.add(AgentIdentity(device_id=device.id, public_key=b"x" * 32))
    db.commit()
    device.online, device.connection_id = True, "still-connected"
    db.commit()

    def unavailable(_device_id):
        raise HTTPException(503, "Broker unavailable")

    monkeypatch.setattr("jump.main.disconnect_revoked_agent", unavailable)
    response = client.post(f"/api/devices/{device.id}/revoke", headers=write_headers())
    assert response.status_code == 503
    db.expire_all()
    assert db.get(Device, device.id).online is False
    assert client.get(f"/api/devices/{device.id}").json()["identity_state"] == "revoked"
    assert client.get(f"/api/internal/identities/{device.id}", headers=BROKER).status_code == 404
    monkeypatch.setattr("jump.main.disconnect_revoked_agent", lambda _device_id: None)
    assert client.post(f"/api/devices/{device.id}/revoke", headers=write_headers()).status_code == 200
    assert db.scalar(
        select(AuditEvent).where(AuditEvent.event_type == "agent_identity_revoked")
    ) is not None
