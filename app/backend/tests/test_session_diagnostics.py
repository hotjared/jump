import json
import uuid
from datetime import timedelta

import pytest
from fastapi.testclient import TestClient
from sqlalchemy import select
from test_audit import login

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
