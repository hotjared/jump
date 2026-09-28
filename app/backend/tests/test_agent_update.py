import uuid
from datetime import timedelta
from unittest.mock import Mock

import httpx
import pytest
from fastapi.testclient import TestClient
from sqlalchemy import select
from test_api import BROKER, ORIGIN, as_user, write_headers

from jump import releases
from jump.db import get_db
from jump.main import app, cfg
from jump.models import (
    AgentIdentity,
    AgentUpdate,
    AuditEvent,
    Device,
    RemoteSession,
    Role,
    now,
)
from jump.releases import newer_release, release_number


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url=ORIGIN) as client:
        yield client
    app.dependency_overrides.clear()


def seed(db, *, capabilities=None):
    device = Device(
        hostname="PROD-SRV01",
        os_family="linux",
        architecture="amd64",
        agent_version="v0.1.2",
        capabilities=capabilities or ["agent_update_v1"],
        addresses=[],
        online=True,
        connection_id=str(uuid.uuid4()),
    )
    db.add(device)
    db.flush()
    db.add(AgentIdentity(device_id=device.id, public_key=uuid.uuid4().bytes * 2))
    db.commit()
    return device


def test_semantic_versions_and_manual_bootstrap(client, db, monkeypatch):
    monkeypatch.setattr(cfg, "jump_agent_version", "v0.1.10")
    assert newer_release("v0.1.9", "v0.1.10")
    assert not newer_release("v0.2.0", "v0.1.10")
    assert not newer_release("dev", "v0.1.10")
    assert not newer_release("v0.1.9-rc.1", "v0.1.10")
    assert release_number("v01.1.1") is None
    device = seed(db, capabilities=["ssh"])
    as_user(client, db)
    status = client.get(f"/api/devices/{device.id}").json()["agent_update"]
    assert status["update_available"] is True
    assert status["remote_update_supported"] is False
    assert (
        client.post(f"/api/devices/{device.id}/agent-update", headers=write_headers()).status_code
        == 409
    )
    device.capabilities = ["agent_update_v1"]
    device.agent_version = "v0.1.10"
    db.commit()
    assert (
        client.get(f"/api/devices/{device.id}").json()["agent_update"]["update_available"] is False
    )
    assert (
        client.post(f"/api/devices/{device.id}/agent-update", headers=write_headers()).status_code
        == 409
    )


def test_update_authorization_conflicts_and_lifecycle(client, db, monkeypatch):
    monkeypatch.setattr(cfg, "jump_agent_version", "v0.1.3")
    device = seed(db)
    path = f"/api/devices/{device.id}/agent-update"
    assert client.post(path).status_code == 403
    user = as_user(client, db, Role.USER)
    assert client.post(path, headers=write_headers()).status_code == 403
    user.role = Role.ADMIN
    db.commit()
    assert client.post(path).status_code == 403
    assert (
        client.post(path, headers=write_headers(), json={"target_version": "v99.0.0"}).status_code
        == 400
    )
    assert (
        client.post(
            path, headers=write_headers(), json={"download_url": "https://evil.example/agent"}
        ).status_code
        == 400
    )
    device.online = False
    db.commit()
    assert client.post(path, headers=write_headers()).status_code == 409
    device.online = True
    device.agent_identity.revoked_at = now()
    db.commit()
    assert client.post(path, headers=write_headers()).status_code == 409
    device.agent_identity.revoked_at = None
    device.architecture = "arm64"
    db.commit()
    assert client.post(path, headers=write_headers()).status_code == 409
    device.architecture = "amd64"
    session = RemoteSession(
        device_id=device.id,
        user_id=user.id,
        credential_id=uuid.uuid4(),
        state="active",
        columns=80,
        rows=24,
    )
    # A real SSH session uses an existing credential; the conflict query only needs this row.
    from jump.models import Credential

    cred = Credential(
        user_id=user.id,
        label="SSH",
        kind="linux_password",
        username="root",
        ciphertext=b"x",
        nonce=b"0" * 12,
    )
    db.add(cred)
    db.flush()
    session.credential_id = cred.id
    db.add(session)
    db.commit()
    assert "Close active sessions" in client.post(path, headers=write_headers()).text
    db.delete(session)
    db.commit()
    monkeypatch.setattr(
        "jump.main.release_asset",
        lambda version, platform: {
            "version": version,
            "platform": platform,
            "architecture": "amd64",
            "download_url": f"https://github.com/hotjared/jump/releases/download/{version}/jump-agent-linux-amd64",
            "sha256": "a" * 64,
        },
    )
    sent = []
    original_post = httpx.Client.post

    def fake_post(self, url, **kwargs):
        if str(url).endswith("/agent-update") and str(url).startswith("http://broker"):
            sent.append(kwargs["json"])
            return Mock(status_code=204, raise_for_status=lambda: None)
        return original_post(self, url, **kwargs)

    monkeypatch.setattr("httpx.Client.post", fake_post)
    response = client.post(path, headers=write_headers())
    assert response.status_code == 201, response.text
    operation = response.json()
    assert "sha256" not in response.text and "download_url" not in response.text
    assert sent[0]["connection_id"] == device.connection_id
    assert client.post(path, headers=write_headers()).status_code == 409
    status_path = f"/api/internal/devices/{device.id}/agent-update-status"

    def report(state, reason=None, connection=None):
        return client.post(
            status_path,
            headers=BROKER,
            json={
                "operation_id": operation["id"],
                "connection_id": connection or device.connection_id,
                "state": state,
                "reason": reason,
            },
        )

    assert report("downloading", connection=str(uuid.uuid4())).status_code == 409
    assert report("downloading").status_code == 200
    assert report("restarting").status_code == 200
    meta = {
        "hostname": device.hostname,
        "os_family": "linux",
        "os_version": "Ubuntu",
        "architecture": "amd64",
        "capabilities": ["agent_update_v1"],
        "addresses": [],
    }
    assert (
        client.post(
            f"/api/internal/devices/{device.id}/connected",
            headers=BROKER,
            json={
                "connection_id": str(uuid.uuid4()),
                "metadata": {**meta, "agent_version": "v0.1.3"},
            },
        ).status_code
        == 200
    )
    db.expire_all()
    assert db.get(AgentUpdate, uuid.UUID(operation["id"])).state == "completed"
    assert "agent_update_completed" in [e.event_type for e in db.scalars(select(AuditEvent))]


def test_rollback_and_timeout(client, db, monkeypatch):
    monkeypatch.setattr(cfg, "jump_agent_version", "v0.1.3")
    device = seed(db)
    user = as_user(client, db)
    op = AgentUpdate(
        device_id=device.id,
        actor_user_id=user.id,
        from_version="v0.1.2",
        target_version="v0.1.3",
        connection_id=device.connection_id,
        state="restarting",
    )
    db.add(op)
    db.commit()
    meta = {
        "hostname": device.hostname,
        "os_family": "linux",
        "os_version": "Ubuntu",
        "architecture": "amd64",
        "capabilities": ["agent_update_v1"],
        "addresses": [],
        "agent_version": "v0.1.2",
    }
    assert (
        client.post(
            f"/api/internal/devices/{device.id}/connected",
            headers=BROKER,
            json={"connection_id": str(uuid.uuid4()), "metadata": meta},
        ).status_code
        == 200
    )
    assert op.state == "failed" and op.failure_reason == "rollback_completed"
    op.created_at = now() - timedelta(minutes=5)
    timed = AgentUpdate(
        device_id=device.id,
        actor_user_id=user.id,
        from_version="v0.1.2",
        target_version="v0.1.3",
        connection_id=device.connection_id,
        state="restarting",
        created_at=now() - timedelta(minutes=3),
        restarting_at=now() - timedelta(minutes=3),
    )
    db.add(timed)
    db.commit()
    client.get(f"/api/devices/{device.id}")
    assert timed.state == "failed" and timed.failure_reason == "reconnect_timeout"


def test_release_checksum_lookup_is_pinned_bounded_and_cached(monkeypatch):
    releases._cache.clear()
    urls = []

    def response(url, status, content=b"", location=None):
        headers = {"location": location} if location else {}
        return httpx.Response(
            status, headers=headers, content=content, request=httpx.Request("GET", url)
        )

    def get(_, url):
        urls.append(url)
        if len(urls) == 1:
            return response(url, 302, location="https://release-assets.githubusercontent.com/asset")
        return response(url, 200, content=("a" * 64 + "  jump-agent-linux-amd64\n").encode())

    monkeypatch.setattr("httpx.Client.get", get)
    metadata = releases.release_asset("v0.1.3", "linux")
    assert metadata["sha256"] == "a" * 64
    assert len(urls) == 2
    releases.release_asset("v0.1.3", "linux")
    assert len(urls) == 2
    releases._cache.clear()

    def bad_redirect(_, url):
        return response(url, 302, location="http://evil.example/checksum")

    monkeypatch.setattr("httpx.Client.get", bad_redirect)
    with pytest.raises(ValueError, match="Untrusted"):
        releases.release_asset("v0.1.3", "linux")
