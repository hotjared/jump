import asyncio
import json
import uuid

import pytest
from fastapi import HTTPException
from fastapi.testclient import TestClient
from sqlalchemy import select
from test_api import ORIGIN, as_user, write_headers
from test_rdp import seed
from test_screen import create

from jump import screen
from jump.db import get_db
from jump.main import app
from jump.models import AuditEvent, RemoteSession, Role


@pytest.fixture
def client(db, monkeypatch):
    monkeypatch.setattr(screen, "release_screen_controller", lambda session: None)
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url=ORIGIN) as connection:
        yield connection
    app.dependency_overrides.clear()


class IdleGateway:
    def __init__(self, sid):
        self.sid = sid
        self.queue = asyncio.Queue()
        self.closed = False
        self.sent = []

    async def send(self, raw):
        frame = json.loads(raw)
        self.sent.append(frame["type"])
        if frame["type"] == "screen_open":
            await self.queue.put(
                json.dumps({"version": 1, "session_id": self.sid, "type": "screen_opened"})
            )

    async def recv(self):
        return await self.queue.get()

    async def close(self):
        self.closed = True


def open_idle(client, db, monkeypatch):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = create(client, device).json()["id"]
    gateway = IdleGateway(sid)

    async def connect(*args, **kwargs):
        return gateway

    monkeypatch.setattr(screen, "ws_connect", connect)
    return device, sid, gateway


@pytest.mark.parametrize("explicit", [False, True])
def test_browser_close_releases_before_finalization(client, db, monkeypatch, explicit):
    device, sid, gateway = open_idle(client, db, monkeypatch)
    released = []

    def release(session):
        assert gateway.closed and gateway.sent[-1] == "screen_close"
        assert session.state == "active"
        released.append(session.id)

    monkeypatch.setattr(screen, "release_screen_controller", release)
    with client.websocket_connect(
        f"/ws/screen-sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
    ) as ws:
        assert ws.receive_json()["state"] == "active"
        if explicit:
            ws.send_json({"type": "screen_close"})
            assert ws.receive_json()["code"] == "session_closed"
    assert released == [uuid.UUID(sid)]
    assert db.get(RemoteSession, uuid.UUID(sid)).state == "closed"
    assert create(client, device).status_code == 201


@pytest.mark.parametrize("respond", [False, True])
def test_heartbeat_independent_of_frames_and_input(client, db, monkeypatch, respond):
    device, sid, gateway = open_idle(client, db, monkeypatch)
    monkeypatch.setattr(screen, "SCREEN_BROWSER_PING_SECONDS", 0.005)
    monkeypatch.setattr(screen, "SCREEN_BROWSER_TIMEOUT_SECONDS", 0.08)
    monkeypatch.setattr(screen, "SCREEN_CONTROL_POLL_SECONDS", 0.001)
    monkeypatch.setattr(screen, "SCREEN_LEASE_REFRESH_SECONDS", 0.005)
    original = screen.refresh_screen_lease
    renewals = []

    def refresh(*args):
        renewals.append(True)
        return original(*args)

    monkeypatch.setattr(screen, "refresh_screen_lease", refresh)
    with client.websocket_connect(
        f"/ws/screen-sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
    ) as ws:
        assert ws.receive_json()["state"] == "active"
        pongs = 0
        while True:
            frame = ws.receive_json()
            if frame["type"] == "status":
                assert frame["code"] == ("session_closed" if respond else "browser_disconnected")
                break
            assert frame["type"] == "screen_ping"
            if respond:
                ws.send_json({"type": "screen_pong", "nonce": frame["nonce"]})
                pongs += 1
                if pongs >= 20:  # Survive beyond the abandoned-browser deadline.
                    ws.send_json({"type": "screen_close"})
    assert renewals and gateway.closed
    assert "screen_pong" not in gateway.sent
    assert create(client, device).status_code == 201


def test_recovery_releases_controller_and_finalizes_once(client, db, monkeypatch):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = create(client, device).json()["id"]
    session = db.get(RemoteSession, uuid.UUID(sid))
    session.state = "active"
    db.commit()
    endpoint = f"/api/devices/{device.id}/screen-sessions/{sid}/close"
    released = []

    def release(current):
        assert current.screen_close_requested_at
        assert create(client, device).status_code == 409
        released.append(current.id)

    monkeypatch.setattr(screen, "release_screen_controller", release)
    assert client.get(f"/api/devices/{device.id}/screen-sessions/current").json() == {"id": sid}
    assert client.post(endpoint, headers=write_headers(), json={}).status_code == 200
    assert len(released) == 1 and session.state == "closed"
    monkeypatch.setattr(screen, "release_screen_controller", lambda current: released.append(current.id))
    assert client.post(endpoint, headers=write_headers(), json={}).status_code == 200
    events = list(db.scalars(select(AuditEvent).where(AuditEvent.event_type == "screen_session_ended")))
    assert len(events) == 1
    assert create(client, device).status_code == 201


def test_failed_release_keeps_reservation_and_retry_is_safe(client, db, monkeypatch):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = create(client, device).json()["id"]
    endpoint = f"/api/devices/{device.id}/screen-sessions/{sid}/close"

    def unavailable(session):
        raise HTTPException(503, "controller still closing")

    monkeypatch.setattr(screen, "release_screen_controller", unavailable)
    assert client.post(endpoint, headers=write_headers(), json={}).status_code == 503
    session = db.get(RemoteSession, uuid.UUID(sid))
    assert session.state == "connecting" and session.screen_close_requested_at
    assert not screen.refresh_screen_lease(db, session.id, device.connection_id)
    assert create(client, device).status_code == 409
    monkeypatch.setattr(screen, "release_screen_controller", lambda session: None)
    assert client.post(endpoint, headers=write_headers(), json={}).status_code == 200
    assert create(client, device).status_code == 201


def test_recovery_authorization_csrf_owner_and_protocol(client, db, monkeypatch):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = create(client, device).json()["id"]
    endpoint = f"/api/devices/{device.id}/screen-sessions/{sid}/close"
    release = []
    monkeypatch.setattr(screen, "release_screen_controller", lambda session: release.append(session.id))
    assert client.post(endpoint, json={}).status_code == 403
    as_user(client, db, role=Role.USER)
    assert client.post(endpoint, headers=write_headers(), json={}).status_code == 403
    as_user(client, db)
    assert client.get(f"/api/devices/{device.id}/screen-sessions/current").json() == {"id": None}
    assert client.post(endpoint, headers=write_headers(), json={}).status_code == 404
    session = db.get(RemoteSession, uuid.UUID(sid))
    user = as_user(client, db)
    session.user_id = user.id
    for protocol in ("ssh", "rdp", "files"):
        session.protocol = protocol
        db.commit()
        assert client.post(endpoint, headers=write_headers(), json={}).status_code == 404
    assert release == []


def test_recovery_wakes_existing_gateway(client, db, monkeypatch):
    device, sid, gateway = open_idle(client, db, monkeypatch)
    monkeypatch.setattr(screen, "SCREEN_CONTROL_POLL_SECONDS", 0.001)
    with client.websocket_connect(
        f"/ws/screen-sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
    ) as ws:
        assert ws.receive_json()["state"] == "active"
        assert client.post(
            f"/api/devices/{device.id}/screen-sessions/{sid}/close",
            headers=write_headers(),
            json={},
        ).status_code == 200
        assert ws.receive_json()["state"] == "closed"
    assert gateway.closed
    ended = list(db.scalars(select(AuditEvent).where(AuditEvent.event_type == "screen_session_ended")))
    assert len(ended) == 1
    assert create(client, device).status_code == 201
