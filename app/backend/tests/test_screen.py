import asyncio
import base64
import json
import uuid
from concurrent.futures import ThreadPoolExecutor
from datetime import UTC, timedelta
from threading import Barrier, Event

import pytest
from fastapi.testclient import TestClient
from sqlalchemy import create_engine, event, select
from sqlalchemy.orm import Session
from starlette.websockets import WebSocketDisconnect
from test_api import BROKER, ORIGIN, as_user, write_headers
from test_rdp import seed

from jump import screen
from jump.db import Base, get_db
from jump.main import app
from jump.models import AuditEvent, RemoteSession, Role, now
from jump.screen import FrameAssembler, jpeg_size, validate_input
from jump.session_diagnostics import record_stage


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
            self.pending_mode = None

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
                self.pending_mode = frame["mode"]
                return
            if frame["type"] == "screen_ack" and self.pending_mode:
                frame = {"type": "screen_mode", "mode": self.pending_mode}
                self.pending_mode = None
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
                if frame["mode"] == "view":
                    # A second frame gives the next transition an ordered barrier.
                    await self.queue.put(
                        json.dumps(
                            {
                                "version": 1,
                                "session_id": sid,
                                "type": "screen_frame",
                                "frame_id": 2,
                                "count": 1,
                                "width": 2,
                                "height": 1,
                                "data": base64.b64encode(
                                    b"\xff\xd8\xff\xc0\x00\x0b\x08\x00\x01\x00\x02\x01\x01\x11\x00\xff\xd9"
                                ).decode(),
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
        ws.send_json({"type": "screen_mode", "mode": "view"})
        ws.send_json({"type": "screen_input", "input": {"action": "key", "key": 68, "down": True}})
        ws.send_json({"type": "screen_ack", "frame_id": 1})
        assert ws.receive_json() == {"type": "screen_mode", "mode": "view"}
        ws.receive_bytes()
        ws.send_json({"type": "screen_input", "input": {"action": "key", "key": 65, "down": True}})
        ws.send_json({"type": "screen_mode", "mode": "control"})
        ws.send_json({"type": "screen_input", "input": {"action": "key", "key": 67, "down": True}})
        # This ordered barrier releases the acknowledgement after the crafted input.
        ws.send_json({"type": "screen_ack", "frame_id": 2})
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


@pytest.mark.parametrize(
    "state,stale_lease,old_connection,status",
    [
        ("active", False, False, 409),
        ("connecting", False, False, 409),
        ("active", True, False, 201),
        ("connecting", True, False, 201),
        ("active", False, True, 201),
        ("connecting", False, True, 201),
    ],
)
def test_screen_creation_lease_recovery_and_history(
    client, db, state, stale_lease, old_connection, status
):
    device = seed(db, capabilities=["screen_control_v1"])
    user = as_user(client, db)
    sid = uuid.UUID(create(client, device).json()["id"])
    session = db.get(RemoteSession, sid)
    session.state = state
    session.created_at = session.attached_at = now() - timedelta(minutes=5)
    session.last_activity_at = now() - timedelta(minutes=3) if stale_lease else now()
    if state == "active":
        session.connected_at = session.attached_at
        record_stage(db, sid, "session_active")
        db.add(
            AuditEvent(
                event_type="screen_session_started",
                actor_user_id=user.id,
                device_id=device.id,
                detail={"session_id": str(sid)},
            )
        )
    original_connection = session.connection_id
    if old_connection:
        device.connection_id = str(uuid.uuid4())
    db.commit()

    response = create(client, device)
    assert response.status_code == status, response.text
    db.refresh(session)
    if status == 409:
        assert session.state == state and session.closed_at is None
        assert not list(
            db.scalars(select(AuditEvent).where(AuditEvent.event_type == "screen_session_failed"))
        )
        return

    assert session.state == "failed" and session.failure_reason == "session_timeout"
    assert session.closed_at and session.attached_at and session.device_name == device.hostname
    assert session.connection_id == original_connection
    replacement = db.get(RemoteSession, uuid.UUID(response.json()["id"]))
    assert replacement.connection_id == device.connection_id
    assert create(client, device).status_code == 409
    trace = next(x for x in client.get("/api/diagnostics/sessions").json() if x["id"] == str(sid))
    assert trace["state"] == "failed" and trace["failure_reason"] == "session_timeout"
    assert {"session_created", "session_failed"} <= {x["stage"] for x in trace["stages"]}
    if state == "active":
        assert "session_active" in {x["stage"] for x in trace["stages"]}
    events = list(
        db.scalars(select(AuditEvent).where(AuditEvent.event_type.like("screen_session_%")))
    )
    assert sum(e.event_type == "screen_session_failed" for e in events) == 1
    assert sum(e.event_type == "screen_session_started" for e in events) == (state == "active")
    failed = next(e for e in events if e.event_type == "screen_session_failed")
    assert failed.detail["session_id"] == str(sid)
    assert failed.detail["reason"] == "session_timeout"


def test_screen_cleanup_leaves_other_protocols_devices_and_history(client, db):
    device = seed(db, capabilities=["screen_control_v1"])
    other = seed(db, capabilities=["screen_control_v1"])
    user = as_user(client, db)
    old = now() - timedelta(minutes=5)
    untouched = [
        RemoteSession(
            device_id=device.id,
            user_id=user.id,
            protocol=protocol,
            state="active",
            columns=80,
            rows=24,
            last_activity_at=old,
        )
        for protocol in ("ssh", "rdp")
    ]
    untouched += [
        RemoteSession(
            device_id=other.id,
            user_id=user.id,
            protocol="screen",
            state="active",
            columns=1920,
            rows=1080,
            last_activity_at=old,
        ),
        RemoteSession(
            device_id=device.id,
            user_id=user.id,
            protocol="screen",
            state="failed",
            failure_reason="capture_failed",
            closed_at=old,
            columns=1920,
            rows=1080,
            last_activity_at=old,
        ),
    ]
    db.add_all(untouched)
    db.commit()
    for s in untouched:
        db.refresh(s)
    original = [(s.id, s.state, s.failure_reason, s.closed_at) for s in untouched]
    assert create(client, device).status_code == 201
    for s, snapshot in zip(untouched, original, strict=True):
        db.refresh(s)
        assert (s.id, s.state, s.failure_reason, s.closed_at) == snapshot


def test_idle_screen_gateway_refreshes_lease_and_stops_on_disconnect(client, db, monkeypatch):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = create(client, device).json()["id"]
    session = db.get(RemoteSession, uuid.UUID(sid))
    refreshed, stopped = Event(), Event()
    renewals = []
    original_refresh = screen.refresh_screen_lease
    original_maintain = screen.maintain_screen_lease

    def track_refresh(*args):
        result = original_refresh(*args)
        renewals.append(session.last_activity_at)
        if len(renewals) >= 2:
            refreshed.set()
        return result

    async def track_lease(*args):
        try:
            return await original_maintain(*args)
        finally:
            stopped.set()

    class IdleGateway:
        def __init__(self):
            self.queue = asyncio.Queue()

        async def send(self, raw):
            frame = json.loads(raw)
            if frame["type"] == "screen_open":
                await self.queue.put(
                    json.dumps(
                        {
                            "version": 1,
                            "session_id": sid,
                            "type": "screen_opened",
                        }
                    )
                )

        async def recv(self):
            return await self.queue.get()

        async def close(self):
            pass

    async def connect(*args, **kwargs):
        return IdleGateway()

    monkeypatch.setattr(screen, "ws_connect", connect)
    monkeypatch.setattr(screen, "SCREEN_LEASE_REFRESH_SECONDS", 0.01)
    monkeypatch.setattr(screen, "refresh_screen_lease", track_refresh)
    monkeypatch.setattr(screen, "maintain_screen_lease", track_lease)
    with client.websocket_connect(
        f"/ws/screen-sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
    ) as ws:
        assert ws.receive_json()["state"] == "active"
        initial = session.last_activity_at
        # No frames or input are exchanged while the live gateway renews twice.
        assert refreshed.wait(2), "idle gateway did not renew its lease"
        assert renewals[0] > initial and renewals[1] > renewals[0]
        ws.send_json({"type": "screen_close"})
        assert ws.receive_json()["state"] == "closed"
    assert stopped.wait(2), "lease task leaked after gateway shutdown"
    db.refresh(session)
    assert session.state == "closed"


def test_screen_lease_cannot_renew_finalized_or_other_connection(client, db, monkeypatch):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = uuid.UUID(create(client, device).json()["id"])
    session = db.get(RemoteSession, sid)
    original = session.last_activity_at
    assert not screen.refresh_screen_lease(db, sid, str(uuid.uuid4()))
    db.refresh(session)
    assert session.last_activity_at.replace(tzinfo=UTC) == original.replace(tzinfo=UTC)
    session.state, session.closed_at = "failed", now()
    session.failure_reason = "session_timeout"
    db.commit()
    assert not screen.refresh_screen_lease(db, sid, device.connection_id)
    db.refresh(session)
    assert session.state == "failed"
    assert session.last_activity_at.replace(tzinfo=UTC) == original.replace(tzinfo=UTC)

    async def lost_lease():
        return await screen.maintain_screen_lease(db, sid, device.connection_id)

    # Avoid wall-clock waiting; the closed row must cause the task to exit.
    monkeypatch.setattr(screen, "SCREEN_LEASE_REFRESH_SECONDS", 0)
    assert asyncio.run(lost_lease()) == "session_timeout"


def test_concurrent_screen_creation_keeps_unique_controller(tmp_path):
    engine = create_engine(
        f"sqlite:///{tmp_path / 'screen-concurrency.db'}",
        connect_args={"check_same_thread": False, "timeout": 10},
    )
    Base.metadata.create_all(engine)

    def independent_db():
        with Session(engine, expire_on_commit=False) as session:
            yield session

    app.dependency_overrides[get_db] = independent_db
    try:
        with (
            Session(engine, expire_on_commit=False) as db,
            TestClient(app, base_url=ORIGIN) as client,
        ):
            device = seed(db, capabilities=["screen_control_v1"])
            as_user(client, db)
            barrier = Barrier(2)

            @event.listens_for(engine, "after_cursor_execute")
            def race_after_busy_check(conn, cursor, statement, parameters, context, executemany):
                if statement.startswith("SELECT remote_sessions.id") and "LIMIT" in statement:
                    # Both requests observe no active controller before either inserts.
                    barrier.wait(timeout=5)

            with ThreadPoolExecutor(max_workers=2) as workers:
                attempts = [workers.submit(create, client, device) for _ in range(2)]
                responses = [attempt.result(timeout=10) for attempt in attempts]
            event.remove(engine, "after_cursor_execute", race_after_busy_check)
            assert sorted(r.status_code for r in responses) == [201, 409]
            live = list(
                db.scalars(
                    select(RemoteSession).where(
                        RemoteSession.device_id == device.id,
                        RemoteSession.protocol == "screen",
                        RemoteSession.state.in_(("connecting", "active")),
                    )
                )
            )
            assert len(live) == 1
            assert next(r for r in responses if r.status_code == 409).json()["detail"] == (
                "A Screen Control session is already active."
            )
    finally:
        app.dependency_overrides.clear()
        engine.dispose()


@pytest.mark.parametrize("when", ["opening", "active"])
def test_expired_gateway_cannot_reactivate_or_duplicate_history(client, db, monkeypatch, when):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = uuid.UUID(create(client, device).json()["id"])
    session = db.get(RemoteSession, sid)

    def expire():
        with Session(db.get_bind(), expire_on_commit=False) as recovery:
            current_device = recovery.get(type(device), device.id)
            current_device.connection_id = str(uuid.uuid4())
            screen.expire_stale_screen_sessions(recovery, current_device)
        # Simulate the gateway's cached ORM object surviving external recovery.
        assert session.state in ("connecting", "active")

    class Gateway:
        def __init__(self):
            self.first = True
            self.queue = asyncio.Queue()

        async def send(self, raw):
            pass

        async def recv(self):
            if self.first:
                self.first = False
                if when == "opening":
                    expire()
                return json.dumps({"version": 1, "session_id": str(sid), "type": "screen_opened"})
            return await self.queue.get()

        async def close(self):
            pass

    async def connect(*args, **kwargs):
        return Gateway()

    async def expire_live_lease(*args):
        expire()
        return "session_timeout"

    monkeypatch.setattr(screen, "ws_connect", connect)
    if when == "active":
        monkeypatch.setattr(screen, "maintain_screen_lease", expire_live_lease)
    with client.websocket_connect(
        f"/ws/screen-sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
    ) as ws:
        if when == "active":
            assert ws.receive_json()["state"] == "active"
        assert ws.receive_json()["code"] == "session_timeout"
    db.refresh(session)
    assert session.state == "failed" and session.failure_reason == "session_timeout"
    events = list(
        db.scalars(select(AuditEvent).where(AuditEvent.event_type.like("screen_session_%")))
    )
    assert sum(e.event_type == "screen_session_started" for e in events) == (when == "active")
    assert sum(e.event_type == "screen_session_failed" for e in events) == 1
    assert create(client, device).status_code == 201


@pytest.mark.parametrize(
    "code",
    [
        "capture_invalid_dimensions",
        "capture_get_dc_failed",
        "capture_create_dc_failed",
        "capture_create_bitmap_failed",
        "capture_select_bitmap_failed",
        "capture_stretch_mode_failed",
        "capture_blit_failed",
        "capture_flush_failed",
        "capture_pixels_failed",
        "capture_encode_failed",
        "unallowlisted Windows error with private content",
    ],
)
def test_capture_stage_diagnostics_remain_safe(client, db, monkeypatch, code):
    device = seed(db, capabilities=["screen_control_v1"])
    as_user(client, db)
    sid = create(client, device).json()["id"]

    class Gateway:
        def __init__(self):
            self.opened = False

        async def send(self, raw):
            pass

        async def recv(self):
            kind = "screen_error" if self.opened else "screen_opened"
            self.opened = True
            return json.dumps(
                {
                    "version": 1,
                    "session_id": sid,
                    "type": kind,
                    "code": code,
                    "extra": "sensitive payload must never be persisted",
                }
            )

        async def close(self):
            pass

    async def connect(*args, **kwargs):
        return Gateway()

    monkeypatch.setattr(screen, "ws_connect", connect)
    expected = code if code in screen.ERRORS else "capture_failed"
    with client.websocket_connect(
        f"/ws/screen-sessions/{sid}", headers={"Host": "localhost", "Origin": ORIGIN}
    ) as ws:
        assert ws.receive_json()["state"] == "active"
        assert ws.receive_json() == {
            "type": "status",
            "state": "closed",
            "code": expected,
            "message": "Desktop capture failed.",
        }
    trace = next(x for x in client.get("/api/diagnostics/sessions").json() if x["id"] == sid)
    assert trace["failure_reason"] == expected
    assert "capture_started" not in {x["stage"] for x in trace["stages"]}
    failure = db.scalar(select(AuditEvent).where(AuditEvent.event_type == "screen_session_failed"))
    assert failure.detail["reason"] == expected
    assert set(failure.detail) == {"session_id", "credential_id", "reason"}
    db.refresh(device)
    assert device.online
