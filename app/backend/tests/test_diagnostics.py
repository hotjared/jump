import asyncio
import json
from datetime import timedelta
from unittest.mock import Mock

import httpx
import pytest
from fastapi.testclient import TestClient
from sqlalchemy.exc import OperationalError
from test_audit import login

from jump.db import get_db
from jump.main import app
from jump.models import (
    AgentUpdate,
    AuditEvent,
    Device,
    FileTransfer,
    RemoteSession,
    Role,
    now,
)


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url="http://localhost:8000") as test_client:
        yield test_client
    app.dependency_overrides.clear()


def probes(monkeypatch, *, broker=True, guacd=True):
    async def broker_get(self, url, **kwargs):
        assert url.endswith("/internal/diagnostics")
        assert kwargs["headers"]["Authorization"].startswith("Bearer ")
        if not broker:
            raise httpx.ConnectError("secret-address")
        return httpx.Response(
            200,
            json={
                "agent_connections": 3,
                "session_routes": 2,
                "file_routes": 1,
                "updates_in_progress": 1,
            },
            request=httpx.Request("GET", url),
        )

    async def connect(*args):
        if not guacd:
            raise OSError("private-host")
        writer = Mock()
        return object(), writer

    monkeypatch.setattr(httpx.AsyncClient, "get", broker_get)
    monkeypatch.setattr(asyncio, "open_connection", connect)


def test_permissions_and_dependency_states(client, db, monkeypatch):
    assert client.get("/api/diagnostics").status_code == 401
    person = login(client, db, Role.USER)
    assert client.get("/api/diagnostics").status_code == 403
    person.role = Role.ADMIN
    db.commit()
    probes(monkeypatch)
    data = client.get("/api/diagnostics").json()
    assert {key: value["status"] for key, value in data["components"].items()} == {
        "api": "healthy",
        "database": "healthy",
        "broker": "healthy",
        "guacd": "healthy",
    }
    assert data["broker_runtime"] == {
        "agent_connections": 3,
        "session_routes": 2,
        "file_routes": 1,
        "updates_in_progress": 1,
    }
    probes(monkeypatch, broker=False, guacd=False)
    data = client.get("/api/diagnostics").json()
    assert data["components"]["broker"]["status"] == "unavailable"
    assert data["components"]["guacd"]["status"] == "unavailable"
    assert data["broker_runtime"] is None
    assert "secret-address" not in json.dumps(data)
    assert "private-host" not in json.dumps(data)


def test_counts_and_safe_newest_failures(client, db, monkeypatch):
    person = login(client, db, Role.ADMIN)
    probes(monkeypatch)
    device = Device(hostname="server", os_family="linux", online=True)
    db.add(device)
    db.flush()
    db.add_all(
        [
            RemoteSession(
                device_id=device.id, user_id=person.id, state="active", columns=80, rows=24
            ),
            RemoteSession(
                device_id=device.id, user_id=person.id, state="closed", columns=80, rows=24
            ),
            FileTransfer(
                device_id=device.id,
                user_id=person.id,
                connection_id="conn",
                direction="upload",
                remote_path="/secret/path",
                filename="safe.txt",
                state="pending",
            ),
            AgentUpdate(
                device_id=device.id,
                from_version="1",
                target_version="2",
                connection_id="conn",
                state="restarting",
            ),
        ]
    )
    events = []
    for n in range(12):
        event = AuditEvent(
            event_type="rdp_session_failed",
            device_id=device.id,
            detail={
                "reason": "guacd_disconnected",
                "password": "do-not-show",
                "remote_path": "/private/secret",
            },
            created_at=now() + timedelta(seconds=n),
        )
        events.append(event)
        db.add(event)
    db.add(AuditEvent(event_type="device_deleted", detail={"password": "do-not-show"}))
    db.commit()
    data = client.get("/api/diagnostics").json()
    assert data["runtime"] == {
        "devices_online": 1,
        "active_sessions": 1,
        "active_file_transfers": 1,
        "active_agent_updates": 1,
    }
    assert len(data["recent_failures"]) == 10
    assert [item["id"] for item in data["recent_failures"]] == [
        str(event.id) for event in reversed(events[2:])
    ]
    assert all(
        e["event_type"] == "rdp_session_failed" and e["device_name"] == "server"
        for e in data["recent_failures"]
    )
    assert all(e["detail"] == {"reason": "guacd_disconnected"} for e in data["recent_failures"])
    assert "do-not-show" not in json.dumps(data)
    assert "/private/secret" not in json.dumps(data)
    assert "test-broker-token" not in json.dumps(data)


def test_database_error_is_controlled(client, db, monkeypatch):
    login(client, db, Role.ADMIN)
    probes(monkeypatch)
    original = db.execute

    def failing(statement, *args, **kwargs):
        if str(statement) == "SELECT 1":
            raise OperationalError("SELECT 1", {}, Exception("secret"))
        return original(statement, *args, **kwargs)

    monkeypatch.setattr(db, "execute", failing)
    data = client.get("/api/diagnostics").json()
    assert data["components"]["database"]["status"] == "unavailable"
    assert data["runtime"] is None
    assert data["recent_failures"] == []
    assert "secret" not in json.dumps(data)
