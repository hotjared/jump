import base64
import json
import uuid

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from fastapi.testclient import TestClient
from itsdangerous import TimestampSigner
from sqlalchemy import func, select

from jump.db import get_db
from jump.main import app
from jump.models import (
    AgentIdentity,
    AuditEvent,
    Credential,
    Device,
    Group,
    Role,
    Tag,
    User,
    device_tags,
    now,
)

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


def test_agent_downloads_are_admin_only_and_have_no_enrollment_token(client, db, monkeypatch):
    assert client.get("/api/agent-downloads").status_code == 401
    user = as_user(client, db, Role.USER)
    assert client.get("/api/agent-downloads").status_code == 403
    user.role = Role.ADMIN
    db.commit()
    monkeypatch.setattr("jump.main.cfg.jump_server_version", "v1.2.3")
    response = client.get("/api/agent-downloads")
    assert response.status_code == 200
    links = response.json()
    assert links["version"] == "v1.2.3"
    assert links["downloads"]["linux"].endswith("/jump-agent-linux-amd64")
    assert links["downloads"]["windows"].endswith("/jump-agent-windows-amd64.exe")
    assert links["checksums"].endswith("/SHA256SUMS")
    assert "token" not in response.text


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
    admin_url = f"/api/devices/{device_id}"
    presence = {"connection_id": "live-connection", "metadata": META}
    assert client.post(url + "/connected", json=presence, headers=BROKER).status_code == 200

    disconnected = []
    monkeypatch.setattr("jump.main.disconnect_revoked_agent", disconnected.append)
    user.role = Role.USER
    db.commit()
    assert client.post(admin_url + "/revoke", headers=write_headers()).status_code == 403
    user.role = Role.ADMIN
    db.commit()
    assert client.post(admin_url + "/revoke").status_code == 403
    response = client.post(admin_url + "/revoke", headers=write_headers())
    assert response.status_code == 200, response.text
    assert response.json()["identity_state"] == "revoked"
    assert response.json()["online"] is False
    assert disconnected == [uuid.UUID(device_id)]
    assert client.get("/api/internal/identities/" + device_id, headers=BROKER).status_code == 404
    assert client.post(url + "/connected", json=presence, headers=BROKER).status_code == 403
    assert client.post(url + "/heartbeat", json=presence, headers=BROKER).status_code == 409
    assert db.get(Device, uuid.UUID(device_id)) is not None
    assert client.get(admin_url).json()["identity_state"] == "revoked"
    assert client.post(admin_url + "/revoke", headers=write_headers()).status_code == 200
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
    assert (
        client.post(f"/api/devices/{device.id}/revoke", headers=write_headers()).status_code == 200
    )
    assert (
        db.scalar(select(AuditEvent).where(AuditEvent.event_type == "agent_identity_revoked"))
        is not None
    )



def test_device_delete_permissions_and_preconditions(client, db):
    device = Device(hostname="delete-me", os_family="linux", capabilities=[], addresses=[])
    db.add(device)
    db.flush()
    identity = AgentIdentity(device_id=device.id, public_key=b"d" * 32)
    db.add(identity)
    db.commit()
    url = f"/api/devices/{device.id}"

    # The write guard rejects a request without a valid browser CSRF session
    # before the route dependency can return Login required.
    assert client.delete(url, headers=write_headers()).status_code == 403

    user = as_user(client, db, Role.USER)
    assert client.delete(url, headers=write_headers()).status_code == 403

    user.role = Role.ADMIN
    db.commit()
    assert client.delete(url).status_code == 403

    response = client.delete(url, headers=write_headers())
    assert response.status_code == 409
    assert "Revoke" in response.json()["detail"]
    db.refresh(identity)
    assert identity.revoked_at is None
    assert db.get(Device, device.id) is not None

    identity.revoked_at = now()
    device.online = True
    device.connection_id = "still-online"
    db.commit()
    response = client.delete(url, headers=write_headers())
    assert response.status_code == 409
    assert "offline" in response.json()["detail"].lower()
    assert db.get(Device, device.id) is not None

    assert client.delete(f"/api/devices/{uuid.uuid4()}", headers=write_headers()).status_code == 404


def test_revoked_offline_device_delete_cascades_owned_data_and_preserves_shared_data(client, db):
    user = as_user(client, db)
    group = Group(name="Shared group")
    tag = Tag(name="Shared tag")
    db.add_all([group, tag])
    db.flush()

    device = Device(
        hostname="old-host",
        display_name="Old Host",
        os_family="linux",
        capabilities=[],
        addresses=[],
        group_id=group.id,
        online=False,
    )
    device.tags = [tag]
    db.add(device)
    db.flush()
    identity = AgentIdentity(device_id=device.id, public_key=b"k" * 32, revoked_at=now())
    credential = Credential(
        device_id=device.id,
        label="Local admin",
        kind="password",
        username="administrator",
        ciphertext=b"ciphertext",
        nonce=b"nonce",
    )
    unrelated_audit = AuditEvent(event_type="unrelated_event", actor_user_id=user.id)
    db.add_all([identity, credential, unrelated_audit])
    db.commit()

    device_id = device.id
    device_uuid = device.device_uuid
    response = client.delete(f"/api/devices/{device_id}", headers=write_headers())
    assert response.status_code == 200
    assert response.json() == {"ok": True}

    db.expire_all()
    assert db.get(Device, device_id) is None
    assert db.scalar(select(AgentIdentity).where(AgentIdentity.device_id == device_id)) is None
    assert db.scalar(select(Credential).where(Credential.device_id == device_id)) is None
    assert db.get(Group, group.id) is not None
    assert db.get(Tag, tag.id) is not None
    assert db.scalar(
        select(func.count()).select_from(device_tags).where(device_tags.c.device_id == device_id)
    ) == 0
    assert db.get(AuditEvent, unrelated_audit.id) is not None

    event = db.scalar(select(AuditEvent).where(AuditEvent.event_type == "device_deleted"))
    assert event is not None
    assert event.actor_user_id == user.id
    assert event.device_id is None
    assert event.detail["device_id"] == str(device_id)
    assert event.detail["device_uuid"] == str(device_uuid)
    assert event.detail["hostname"] == "old-host"
    assert event.detail["display_name"] == "Old Host"
    assert event.request_id
    assert client.get(f"/api/internal/identities/{device_id}", headers=BROKER).status_code == 404
