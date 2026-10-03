from datetime import timedelta

import pytest
from fastapi.testclient import TestClient
from test_api import BROKER, META, as_user
from test_files import device

from jump import main
from jump.db import get_db
from jump.models import AuditEvent, FileTransfer


@pytest.fixture
def client(db):
    main.app.dependency_overrides[get_db] = lambda: db
    with TestClient(main.app, base_url="http://localhost:8000") as client:
        yield client
    main.app.dependency_overrides.clear()


@pytest.mark.parametrize("service", ["root", "AD\\PROD-DC01$", "NT AUTHORITY\\SYSTEM"])
def test_old_service_identity_is_not_current_user(client, db, service):
    as_user(client, db)
    target = device(db)
    target.current_user = service
    db.commit()
    result = client.get(f"/api/devices/{target.id}").json()
    assert result["current_user"] is None
    assert result["agent_service_user"] == service


def test_current_user_refreshes_on_heartbeat_and_clears_on_logoff(client, db):
    as_user(client, db)
    target = device(db)
    url = f"/api/internal/devices/{target.id}/heartbeat"
    data = {**META, "current_user": "root", "interactive_user": "jared"}
    presence = {"connection_id": target.connection_id, "metadata": data}
    assert client.post(url, json=presence, headers=BROKER).status_code == 200
    db.refresh(target)
    assert client.get(f"/api/devices/{target.id}").json()["current_user"] == "jared"
    # An old/replaced connection cannot change endpoint state.
    assert (
        client.post(url, json={**presence, "connection_id": "old"}, headers=BROKER).status_code
        == 409
    )
    data["interactive_user"] = None
    assert client.post(url, json=presence, headers=BROKER).status_code == 200
    db.refresh(target)
    assert client.get(f"/api/devices/{target.id}").json()["current_user"] is None
    # Older agents omit the new field and still connect successfully.
    data.pop("interactive_user")
    target.interactive_user = "stale"
    db.commit()
    connected = f"/api/internal/devices/{target.id}/connected"
    assert client.post(connected, json=presence, headers=BROKER).status_code == 200
    assert client.get(f"/api/devices/{target.id}").json()["current_user"] is None
    assert (
        client.post(url, json={"connection_id": target.connection_id}, headers=BROKER).status_code
        == 200
    )


def test_only_live_and_recent_transfers_are_listed_without_deleting_history(client, db):
    user = as_user(client, db)
    target = device(db)
    old = main.now() - timedelta(minutes=10)
    transfers = []
    for state in ("completed", "failed", "cancelled"):
        transfer = main.file_record(db, user, target, "upload", "/tmp", state, 12)
        main.file_finish(db, transfer, state, "transfer_failed" if state == "failed" else None)
        transfer.completed_at = old
        transfers.append(transfer)
    recent = main.file_record(db, user, target, "download", "/tmp", "recent", 12)
    main.file_finish(db, recent, "completed")
    live = main.file_record(db, user, target, "upload", "/tmp", "active", 12)
    live.created_at = old
    db.commit()
    url = f"/api/devices/{target.id}/file-transfers"
    assert {row["id"] for row in client.get(url).json()} == {str(recent.id), str(live.id)}
    recent.completed_at = main.now() - timedelta(seconds=61)
    db.commit()
    assert {row["id"] for row in client.get(url).json()} == {str(live.id)}
    assert all(db.get(FileTransfer, transfer.id) is not None for transfer in transfers)
    history = client.get("/api/diagnostics/file-transfers").json()
    assert all(str(transfer.id) in str(history) for transfer in transfers)
    assert (
        db.query(AuditEvent)
        .filter(AuditEvent.event_type.in_(("file_upload_completed", "file_upload_failed")))
        .count()
        >= 2
    )
    # Stale active transfers still fail visibly, then age out without deletion.
    live.last_activity_at = old
    db.commit()
    assert client.get(url).json()[0]["state"] == "failed"
    assert db.get(FileTransfer, live.id).completed_at is not None
