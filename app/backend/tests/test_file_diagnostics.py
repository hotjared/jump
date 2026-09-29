import json
import uuid
from datetime import timedelta
from pathlib import Path
from types import SimpleNamespace

import pytest
from alembic.config import Config
from fastapi.testclient import TestClient
from sqlalchemy import create_engine, event, text
from test_api import as_user

from alembic import command
from jump import main
from jump.db import Base, get_db
from jump.file_diagnostics import record_file_stage
from jump.models import Device, FileTransfer, FileTransferDiagnosticEvent, Role, now


@pytest.fixture
def client(db):
    main.app.dependency_overrides[get_db] = lambda: db
    with TestClient(main.app, base_url="http://localhost:8000") as test_client:
        yield test_client
    main.app.dependency_overrides.clear()


def test_allowlist_duplicate_order_api_and_privacy(client, db):
    assert client.get("/api/diagnostics/file-transfers").status_code == 401
    user = as_user(client, db, Role.USER)
    assert client.get("/api/diagnostics/file-transfers").status_code == 403
    user.role = Role.ADMIN
    device = Device(hostname="docker01", display_name="Former docker01", os_family="linux")
    db.add(device)
    db.flush()
    older = FileTransfer(
        id=uuid.uuid4(),
        user_id=user.id,
        device_id=device.id,
        direction="list",
        connection_id="private-connection",
        remote_path="/secret/list",
        filename="",
        state="completed",
    )
    first = FileTransfer(
        id=uuid.uuid4(),
        user_id=user.id,
        device_id=device.id,
        device_name="Former docker01",
        direction="download",
        connection_id="private-connection",
        remote_path="/private/backup.zip",
        filename="/private/backup.zip",
        state="failed",
        failure_reason="password=secret",
        created_at=now() - timedelta(minutes=1),
    )
    second = FileTransfer(
        id=uuid.uuid4(),
        user_id=user.id,
        device_id=device.id,
        direction="upload",
        connection_id="private-connection",
        remote_path="/secret/config.txt",
        filename="config.txt",
        state="cancelled",
        failure_reason="transfer_cancelled",
    )
    db.add_all([older, first, second])
    db.flush()
    for stage in ("transfer_created", "broker_connected", "transfer_failed"):
        record_file_stage(db, first.id, stage)
    record_file_stage(db, first.id, "transfer_failed")
    with pytest.raises(ValueError):
        record_file_stage(db, first.id, "raw agent payload")
    record_file_stage(db, second.id, "transfer_cancelled")
    db.commit()
    data = client.get("/api/diagnostics/file-transfers").json()
    assert [row["id"] for row in data] == [str(second.id), str(first.id)]
    assert [stage["stage"] for stage in data[1]["stages"]] == [
        "transfer_created",
        "broker_connected",
        "transfer_failed",
    ]
    assert data[1]["filename"] == "backup.zip"
    assert data[1]["failure_reason"] is None
    assert data[0]["failure_reason"] == "transfer_cancelled"
    assert not any(
        value in json.dumps(data)
        for value in ("/private", "/secret", "private-connection", "password=secret")
    )


def test_deleted_device_retains_history_and_old_snapshot_fallback(client, db):
    user = as_user(client, db, Role.ADMIN)
    device = Device(hostname="hostname", os_family="linux")
    db.add(device)
    db.flush()
    transfers = [
        FileTransfer(
            id=uuid.uuid4(),
            user_id=user.id,
            device_id=device.id,
            device_name=name,
            direction="download",
            connection_id="unused",
            remote_path="/secret/file",
            filename="file",
            state="completed",
        )
        for name in ("Former hostname", None)
    ]
    db.add_all(transfers)
    db.commit()
    db.delete(device)
    db.commit()
    db.expire_all()
    data = client.get("/api/diagnostics/file-transfers").json()
    assert {row["device"]["name"] for row in data} == {"Former hostname", "Deleted device"}
    assert all(row["device"]["id"] is None for row in data)
    assert all(row["stages"] == [] for row in data)


def test_event_rows_cascade_with_transfer(tmp_path):
    engine = create_engine(f"sqlite:///{tmp_path / 'cascade.db'}")

    @event.listens_for(engine, "connect")
    def foreign_keys(connection, record):
        connection.execute("PRAGMA foreign_keys=ON")

    Base.metadata.create_all(engine)
    assert FileTransferDiagnosticEvent.__table__.c.transfer_id.foreign_keys
    assert (
        next(iter(FileTransferDiagnosticEvent.__table__.c.transfer_id.foreign_keys)).ondelete
        == "CASCADE"
    )
    engine.dispose()


def test_migration_preserves_rows_and_cascades_events(tmp_path, monkeypatch):
    database_url = f"sqlite:///{tmp_path / 'migration.db'}"
    monkeypatch.setattr("jump.config.settings", lambda: SimpleNamespace(database_url=database_url))
    config = Config(str(Path(__file__).parents[1] / "alembic.ini"))
    config.set_main_option("script_location", str(Path(__file__).parents[1] / "alembic"))
    command.upgrade(config, "0008")
    engine = create_engine(database_url)
    user_id, device_id, transfer_id = (uuid.uuid4().hex for _ in range(3))
    with engine.begin() as connection:
        connection.execute(
            text(
                "INSERT INTO users (id, oidc_issuer, oidc_subject, email, display_name, role, created_at) VALUES (:id, 'issuer', 'subject', 'a@example.com', 'Admin', 'admin', CURRENT_TIMESTAMP)"
            ),
            {"id": user_id},
        )
        connection.execute(
            text(
                "INSERT INTO devices (id, device_uuid, hostname, os_family, os_version, architecture, agent_version, capabilities, addresses, online, enrolled_at, created_at, updated_at) VALUES (:id, :device_uuid, 'old-host', 'linux', '', '', '', '[]', '[]', 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"
            ),
            {"id": device_id, "device_uuid": uuid.uuid4().hex},
        )
        connection.execute(
            text(
                "INSERT INTO file_transfers (id, user_id, device_id, connection_id, direction, remote_path, filename, transferred_bytes, state, created_at, last_activity_at) VALUES (:id, :user_id, :device_id, 'unused', 'download', '/private/file', 'file', 0, 'completed', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"
            ),
            {"id": transfer_id, "user_id": user_id, "device_id": device_id},
        )
    engine.dispose()
    command.upgrade(config, "0009")
    engine = create_engine(database_url)
    with engine.begin() as connection:
        connection.execute(text("PRAGMA foreign_keys=ON"))
        assert connection.execute(
            text("SELECT device_id, device_name FROM file_transfers WHERE id=:id"),
            {"id": transfer_id},
        ).one() == (device_id, None)
        connection.execute(
            text(
                "INSERT INTO file_transfer_diagnostic_events (id, transfer_id, stage, created_at) VALUES (:id, :transfer_id, 'transfer_created', CURRENT_TIMESTAMP)"
            ),
            {"id": uuid.uuid4().hex, "transfer_id": transfer_id},
        )
        connection.execute(text("DELETE FROM devices WHERE id=:id"), {"id": device_id})
        assert (
            connection.execute(
                text("SELECT device_id FROM file_transfers WHERE id=:id"), {"id": transfer_id}
            ).scalar_one()
            is None
        )
        assert (
            connection.execute(
                text("SELECT COUNT(*) FROM file_transfer_diagnostic_events")
            ).scalar_one()
            == 1
        )
        connection.execute(text("DELETE FROM file_transfers WHERE id=:id"), {"id": transfer_id})
        assert (
            connection.execute(
                text("SELECT COUNT(*) FROM file_transfer_diagnostic_events")
            ).scalar_one()
            == 0
        )
    engine.dispose()
