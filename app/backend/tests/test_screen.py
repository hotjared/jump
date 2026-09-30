import asyncio
import base64
import json
import uuid
from datetime import timedelta

import pytest
from fastapi.testclient import TestClient
from sqlalchemy import select
from starlette.websockets import WebSocketDisconnect
from test_api import BROKER, ORIGIN, as_user, write_headers
from test_rdp import seed

from jump.db import get_db
from jump.main import app
from jump.models import AuditEvent, RemoteSession, Role, now
from jump.screen import FrameAssembler, jpeg_size, validate_input


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url=ORIGIN) as client:
        yield client
    app.dependency_overrides.clear()


def create(client, device):
    return client.post(
        f"/api/devices/{device.id}/screen-sessions", headers=write_headers(), json={}
    )


def test_screen_creation_constraints_and_connection_snapshot(client, db):
    device = seed(db, capabilities=["screen_control_v1"])
    assert create(client, device).status_code == 403
    as_user(client, db, role=Role.USER)
    assert create(client, device).status_code == 403
    user = as_user(client, db)
    for attr, invalid in (("online", False), ("os_family", "linux"), ("capabilities", [])):
        previous = getattr(device, attr)
        setattr(device, attr, invalid)
        db.commit()
        assert create(client, device).status_code == 409
        setattr(device, attr, previous)
        db.commit()
    device.agent_identity.revoked_at = now()
    db.commit()
    assert create(client, device).status_code == 409
    device.agent_identity.revoked_at = None
    device.display_name = "Console PC"
    db.commit()
    response = create(client, device)
    assert response.status_code == 201, response.text
    session = db.get(RemoteSession, uuid.UUID(response.json()["id"]))
    assert session.protocol == "screen" and session.credential_id is None
    assert session.device_name == "Console PC" and session.user_id == user.id
    assert session.connection_id == device.connection_id
    assert create(client, device).status_code == 409
    session.created_at = now() - timedelta(minutes=2)
    db.commit()
    assert create(client, device).status_code == 201
    assert session.state == "failed" and session.failure_reason == "session_timeout"


def test_screen_private_authorization_owner_exact_connection_and_capability(client, db):
    device = seed(db, capabilities=["screen_control_v1"])
    user = as_user(client, db)
    sid = create(client, device).json()["id"]
    session = db.get(RemoteSession, uuid.UUID(sid))
    path = f"/api/internal/screen-streams/{sid}/authorize"
    params = {
        "device_id": str(device.id),
        "connection_id": device.connection_id,
        "user_id": str(user.id),
    }
    assert client.get(path, params=params).status_code == 401
    assert client.get(path, params=params, headers=BROKER).status_code == 403
    session.attached_at = now()
    db.commit()
    assert client.get(path, params=params, headers=BROKER).status_code == 200
    for field in params:
        assert (
            client.get(
                path, params={**params, field: str(uuid.uuid4())}, headers=BROKER
            ).status_code
            == 403
        )
    device.connection_id = str(uuid.uuid4())
    db.commit()
    assert (
        client.get(
            path, params={**params, "connection_id": device.connection_id}, headers=BROKER
        ).status_code
        == 403
    )


def test_screen_browser_origin_and_owner_before_claim(client, db):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = create(client, device).json()["id"]
    for origin in ("", "https://evil.example"):
        with pytest.raises(WebSocketDisconnect):
            with client.websocket_connect(
                f"/ws/screen-sessions/{sid}", headers={"Host": "localhost", "Origin": origin}
            ):
                pass
    as_user(client, db)
    with pytest.raises(WebSocketDisconnect):
        with client.websocket_connect(
            f"/ws/screen-sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
        ):
            pass
    assert db.get(RemoteSession, uuid.UUID(sid)).attached_at is None


def test_screen_lifecycle_diagnostics_audit_and_view_mode(client, db, monkeypatch):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = create(client, device).json()["id"]

    class Gateway:
        def __init__(self):
            self.queue = asyncio.Queue()
            self.sent = []

        async def send(self, raw):
            frame = json.loads(raw)
            self.sent.append(frame)
            if frame["type"] == "screen_open":
                await self.queue.put(
                    json.dumps({"version": 1, "session_id": sid, "type": "screen_opened"})
                )
                jpeg = b"\xff\xd8\xff\xc0\x00\x0b\x08\x00\x01\x00\x02\x01\x01\x11\x00\xff\xd9"
                await self.queue.put(
                    json.dumps(
                        {
                            "version": 1,
                            "session_id": sid,
                            "type": "screen_frame",
                            "frame_id": 1,
                            "count": 1,
                            "width": 2,
                            "height": 1,
                            "data": base64.b64encode(jpeg).decode(),
                        }
                    )
                )
            if frame["type"] == "screen_mode":
                await self.queue.put(
                    json.dumps(
                        {
                            "version": 1,
                            "session_id": sid,
                            "type": "screen_mode",
                            "mode": frame["mode"],
                        }
                    )
                )

        async def recv(self):
            return await self.queue.get()

        async def close(self):
            pass

    gateway = Gateway()

    async def connect(url, **kwargs):
        assert f"connection_id={device.connection_id}" in url and f"device_id={device.id}" in url
        assert kwargs["max_size"] == 65536
        return gateway

    monkeypatch.setattr("jump.screen.ws_connect", connect)
    with client.websocket_connect(
        f"/ws/screen-sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
    ) as ws:
        assert ws.receive_json()["state"] == "active"
        packet = ws.receive_bytes()
        assert packet[12:].startswith(b"\xff\xd8")
        ws.send_json({"type": "screen_ack", "frame_id": 1})
        ws.send_json({"type": "screen_mode", "mode": "view"})
        assert ws.receive_json() == {"type": "screen_mode", "mode": "view"}
        ws.send_json({"type": "screen_input", "input": {"action": "key", "key": 65, "down": True}})
        ws.send_json({"type": "screen_mode", "mode": "control"})
        assert ws.receive_json()["mode"] == "control"
        ws.send_json({"type": "screen_input", "input": {"action": "key", "key": 66, "down": True}})
        ws.send_json({"type": "screen_close"})
        assert ws.receive_json()["state"] == "closed"
    assert [f["input"]["key"] for f in gateway.sent if f["type"] == "screen_input"] == [66]
    session = db.get(RemoteSession, uuid.UUID(sid))
    assert session.state == "closed" and session.closed_at
    trace = client.get("/api/diagnostics/sessions").json()[0]
    assert trace["protocol"] == "screen"
    assert {"interactive_session_found", "capture_started", "session_closed"} <= {
        x["stage"] for x in trace["stages"]
    }
    events = list(
        db.scalars(select(AuditEvent).where(AuditEvent.event_type.like("screen_session_%")))
    )
    assert [e.event_type for e in events] == ["screen_session_started", "screen_session_ended"]
    assert all(set(e.detail) <= {"session_id", "credential_id", "reason"} for e in events)


@pytest.mark.parametrize(
    "value",
    [
        {"action": "shell"},
        {"action": "move", "x": -1},
        {"action": "key", "key": True},
        {"action": "wheel", "delta": 1201},
        {"action": "button", "button": 3},
        {"action": "release", "key": 65},
    ],
)
def test_invalid_input(value):
    with pytest.raises(ValueError):
        validate_input(value)


def test_bounded_assembler_order_and_jpeg_dimensions():
    jpeg = b"\xff\xd8\xff\xc0\x00\x0b\x08\x00\x01\x00\x02\x01\x01\x11\x00\xff\xd9"
    assert jpeg_size(jpeg) == (2, 1)
    frame = {
        "frame_id": 1,
        "count": 1,
        "width": 2,
        "height": 1,
        "data": base64.b64encode(jpeg).decode(),
    }
    assembled = FrameAssembler()
    assert assembled.add(frame)[12:] == jpeg
    with pytest.raises(ValueError):
        assembled.add(frame)
    for change in (
        {"count": 33},
        {"width": 1921},
        {"index": 1},
        {"frame_id": True},
        {"width": 1},
        {"data": "invalid base64"},
    ):
        with pytest.raises(ValueError):
            FrameAssembler().add({**frame, **change})
