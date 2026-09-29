import json
import uuid
from datetime import timedelta
from pathlib import Path
from types import SimpleNamespace

import pytest
from alembic.config import Config
from fastapi.testclient import TestClient
from sqlalchemy import create_engine, select, text
from test_audit import login

from alembic import command
from jump.db import get_db
from jump.main import app, finish_remote_session
from jump.models import Device, RemoteSession, RemoteSessionDiagnosticEvent, Role, now
from jump.session_diagnostics import record_stage


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url="http://localhost:8000") as test_client:
        yield test_client
    app.dependency_overrides.clear()


def test_stages_are_allowlisted_ordered_and_not_duplicated(client, db):
    assert client.get("/api/diagnostics/sessions").status_code == 401
    user = login(client, db, Role.USER)
    assert client.get("/api/diagnostics/sessions").status_code == 403
    user.role = Role.ADMIN
    device = Device(hostname="server", os_family="linux")
    db.add(device)
    db.flush()
    first = RemoteSession(
        id=uuid.uuid4(),
        device_id=device.id,
        user_id=user.id,
        protocol="ssh",
        columns=80,
        rows=24,
        request_id="safe-request",
    )
    second = RemoteSession(
        id=uuid.uuid4(),
        device_id=device.id,
        user_id=user.id,
        protocol="rdp",
        columns=80,
        rows=24,
        created_at=now() + timedelta(seconds=1),
    )
    db.add_all([first, second])
    db.flush()
    for stage in (
        "session_created",
        "browser_attached",
        "broker_connected",
        "agent_stream_opened",
        "host_key_verified",
        "session_active",
    ):
        record_stage(db, first.id, stage)
    record_stage(db, first.id, "session_active")
    with pytest.raises(ValueError):
        record_stage(db, first.id, "password=secret")
    first.state = "active"
    finish_remote_session(db, first, "session_closed")
    second.failure_reason = "arbitrary exception password=secret"
    second.state = "failed"
    db.commit()
    assert (
        len(
            db.scalars(
                select(RemoteSessionDiagnosticEvent).where(
                    RemoteSessionDiagnosticEvent.session_id == first.id
                )
            ).all()
        )
        == 7
    )
    response = client.get("/api/diagnostics/sessions")
    assert response.status_code == 200
    data = response.json()
    assert [row["id"] for row in data] == [str(second.id), str(first.id)]
    assert [event["stage"] for event in data[1]["stages"]] == [
        "session_created",
        "browser_attached",
        "broker_connected",
        "agent_stream_opened",
        "host_key_verified",
        "session_active",
        "session_closed",
    ]
    assert data[1]["request_id"] == "safe-request"
    assert data[0]["failure_reason"] is None
    assert data[0]["device"]["name"] == "server"
    assert not any(
        key in json.dumps(data)
        for key in ("password", "credential_id", "connection_id", "remote_path")
    )


def test_unknown_stored_stage_is_not_returned(client, db):
    user = login(client, db, Role.ADMIN)
    device = Device(hostname="former", os_family="linux")
    db.add(device)
    db.flush()
    session = RemoteSession(device_id=device.id, user_id=user.id, columns=80, rows=24)
    db.add(session)
    db.flush()
    db.add(RemoteSessionDiagnosticEvent(session_id=session.id, stage="untrusted_payload"))
    db.commit()
    data = client.get("/api/diagnostics/sessions").json()
    assert data[0]["stages"] == []


def test_device_deletion_preserves_history_and_uses_name_snapshot(client, db):
    user = login(client, db, Role.ADMIN)
    device = Device(hostname="prod-dc01", display_name="Former PROD-DC01", os_family="windows")
    db.add(device)
    db.flush()
    session = RemoteSession(
        device_id=device.id,
        device_name=device.display_name or device.hostname,
        user_id=user.id,
        protocol="rdp",
        columns=1280,
        rows=720,
    )
    old_session = RemoteSession(device_id=device.id, user_id=user.id, columns=80, rows=24)
    db.add_all([session, old_session])
    db.flush()
    record_stage(db, session.id, "session_created")
    db.commit()

    device.display_name = "Renamed while live"
    db.commit()
    live = {item["id"]: item for item in client.get("/api/diagnostics/sessions").json()}
    assert live[str(session.id)]["device"]["name"] == "Renamed while live"
    assert live[str(old_session.id)]["device"]["name"] == "Renamed while live"

    db.delete(device)
    db.commit()
    db.expire_all()
    assert db.get(RemoteSession, session.id).device_id is None
    deleted = {item["id"]: item for item in client.get("/api/diagnostics/sessions").json()}
    assert deleted[str(session.id)]["device"] == {"id": None, "name": "Former PROD-DC01"}
    assert [event["stage"] for event in deleted[str(session.id)]["stages"]] == ["session_created"]
    assert deleted[str(old_session.id)]["device"] == {"id": None, "name": "Deleted device"}

    db.delete(db.get(RemoteSession, session.id))
    db.commit()
    assert (
        db.scalars(
            select(RemoteSessionDiagnosticEvent).where(
                RemoteSessionDiagnosticEvent.session_id == session.id
            )
        ).all()
        == []
    )


def test_migration_keeps_existing_session_rows(tmp_path, monkeypatch):
    database_url = f"sqlite:///{tmp_path / 'migration.db'}"
    monkeypatch.setattr("jump.config.settings", lambda: SimpleNamespace(database_url=database_url))
    config = Config(str(Path(__file__).parents[1] / "alembic.ini"))
    config.set_main_option("script_location", str(Path(__file__).parents[1] / "alembic"))
    command.upgrade(config, "0007")
    engine = create_engine(database_url)
    user_id, device_id, session_id = (uuid.uuid4().hex for _ in range(3))
    with engine.begin() as connection:
        connection.execute(
            text(
                "INSERT INTO users (id, oidc_issuer, oidc_subject, email, display_name, role, created_at) VALUES (:id, 'issuer', 'subject', 'a@example.com', 'Admin', 'admin', CURRENT_TIMESTAMP)"
            ),
            {"id": user_id},
        )
        connection.execute(
            text(
                "INSERT INTO devices (id, device_uuid, hostname, os_family, os_version, architecture, agent_version, capabilities, addresses, online, enrolled_at, created_at, updated_at) VALUES (:id, :device_uuid, 'older-host', 'linux', '', '', '', '[]', '[]', 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"
            ),
            {"id": device_id, "device_uuid": uuid.uuid4().hex},
        )
        connection.execute(
            text(
                "INSERT INTO remote_sessions (id, device_id, user_id, state, protocol, columns, rows, dpi, created_at, last_activity_at) VALUES (:id, :device_id, :user_id, 'closed', 'ssh', 80, 24, 96, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"
            ),
            {"id": session_id, "device_id": device_id, "user_id": user_id},
        )
    engine.dispose()

    command.upgrade(config, "0008")
    engine = create_engine(database_url)
    with engine.begin() as connection:
        connection.execute(text("PRAGMA foreign_keys=ON"))
        before = connection.execute(
            text("SELECT device_id, device_name FROM remote_sessions WHERE id=:id"),
            {"id": session_id},
        ).one()
        assert before == (device_id, None)
        connection.execute(text("DELETE FROM devices WHERE id=:id"), {"id": device_id})
        after = connection.execute(
            text("SELECT device_id, device_name FROM remote_sessions WHERE id=:id"),
            {"id": session_id},
        ).one()
        assert after == (None, None)
    engine.dispose()
