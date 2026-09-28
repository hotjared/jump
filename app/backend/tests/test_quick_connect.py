import uuid

import pytest
from fastapi.testclient import TestClient
from sqlalchemy import select
from test_api import ORIGIN, as_user, write_headers

from jump.db import get_db
from jump.main import app
from jump.models import Credential, Device, QuickConnectPreference, Role


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url=ORIGIN) as client:
        yield client
    app.dependency_overrides.clear()


def seed(db, user, os_family="windows", kind="windows_password"):
    device = Device(
        hostname="host", os_family=os_family, capabilities=["rdp_tunnel_v1", "ssh_terminal_v1"]
    )
    db.add(device)
    db.flush()
    credential = Credential(
        user_id=user.id,
        label="admin",
        kind=kind,
        username="user",
        ciphertext=b"encrypted",
        nonce=b"0" * 12,
    )
    db.add(credential)
    db.commit()
    return device, credential


def put(client, device, credential, protocol="rdp"):
    return client.put(
        f"/api/devices/{device.id}/quick-connect-preferences",
        headers=write_headers(),
        json={"protocol": protocol, "credential_id": str(credential.id), "preferred": True},
    )


def test_user_scope_validation_and_update(client, db):
    user = as_user(client, db, Role.USER)
    device, credential = seed(db, user)
    assert client.get("/api/quick-connect-preferences").status_code == 403
    assert put(client, device, credential).status_code == 403
    user.role = Role.ADMIN
    db.commit()
    assert put(client, device, credential).status_code == 200
    assert put(client, device, credential).status_code == 200
    assert len(db.scalars(select(QuickConnectPreference)).all()) == 1
    assert client.get("/api/quick-connect-preferences").json() == [
        {
            "device_id": str(device.id),
            "protocol": "rdp",
            "credential_id": str(credential.id),
            "preferred": True,
        }
    ]
    assert "encrypted" not in client.get("/api/quick-connect-preferences").text

    other = as_user(client, db)
    assert client.get("/api/quick-connect-preferences").json() == []
    assert put(client, device, credential).status_code == 400
    assert (
        db.get(QuickConnectPreference, (user.id, device.id, "rdp")).credential_id == credential.id
    )
    assert db.get(QuickConnectPreference, (other.id, device.id, "rdp")) is None
    alien_device, alien_credential = seed(db, other)
    assert put(client, device, alien_credential).status_code == 200
    assert put(client, alien_device, credential).status_code == 400
    assert (
        client.put(
            f"/api/devices/{uuid.uuid4()}/quick-connect-preferences",
            headers=write_headers(),
            json={"protocol": "rdp", "credential_id": str(credential.id)},
        ).status_code
        == 404
    )


def test_type_os_and_stale_credential(client, db):
    user = as_user(client, db)
    windows, password = seed(db, user)
    linux, ssh_key = seed(db, user, "linux", "linux_ssh_key")
    assert put(client, windows, password, "ssh").status_code == 400
    assert put(client, linux, ssh_key, "rdp").status_code == 400
    assert put(client, linux, ssh_key, "ssh").status_code == 200
    assert put(client, windows, ssh_key).status_code == 400
    assert put(client, linux, password, "ssh").status_code == 400
    wrong = Credential(
        user_id=user.id,
        label="wrong",
        kind="linux_password",
        username="user",
        ciphertext=b"x",
        nonce=b"0" * 12,
    )
    db.add(wrong)
    db.commit()
    assert put(client, windows, wrong).status_code == 400
    assert put(client, linux, ssh_key, "ssh").status_code == 200
    db.delete(ssh_key)
    db.commit()
    assert client.get("/api/quick-connect-preferences").json() == []
    assert put(client, linux, ssh_key, "ssh").status_code == 400


def test_deleting_device_and_user_cleans_preferences(client, db):
    user = as_user(client, db)
    device, credential = seed(db, user)
    assert put(client, device, credential).status_code == 200
    db.delete(device)
    db.commit()
    assert db.scalars(select(QuickConnectPreference)).all() == []
    next_device, next_credential = seed(db, user)
    assert put(client, next_device, next_credential).status_code == 200
    db.delete(user)
    db.commit()
    assert db.scalars(select(QuickConnectPreference)).all() == []
