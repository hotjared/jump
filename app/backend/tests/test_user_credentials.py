import base64
import json
import uuid

from fastapi.testclient import TestClient
from itsdangerous import TimestampSigner
from test_api import ORIGIN, as_user, write_headers

from jump.db import get_db
from jump.main import app
from jump.models import AgentIdentity, Credential, Device


def switch_user(client, user):
    state = base64.b64encode(json.dumps({"uid": str(user.id), "csrf": "test-csrf"}).encode())
    client.cookies.set(
        "jump_session",
        TimestampSigner("test-session-secret-at-least-32-characters").sign(state).decode(),
    )


def test_private_reusable_credentials(db):
    app.dependency_overrides[get_db] = lambda: db
    try:
        with TestClient(app, base_url=ORIGIN) as client:
            owner = as_user(client, db)
            devices = []
            for family, cap in [("windows", "rdp_tunnel_v1")] * 2 + [
                ("linux", "ssh_terminal_v1")
            ] * 2:
                device = Device(
                    hostname=str(uuid.uuid4()),
                    os_family=family,
                    capabilities=[cap],
                    addresses=[],
                    online=True,
                    connection_id=str(uuid.uuid4()),
                )
                db.add(device)
                db.flush()
                db.add(AgentIdentity(device_id=device.id, public_key=uuid.uuid4().bytes * 2))
                devices.append(device)
            db.commit()
            ids = []
            for kind in ("windows_password", "linux_ssh_key"):
                response = client.post(
                    "/api/credentials",
                    headers=write_headers(),
                    json={
                        "label": kind,
                        "kind": kind,
                        "username": "admin",
                        "secret": "top-secret",
                    },
                )
                assert response.status_code == 201
                assert set(response.json()) == {"id", "label", "kind", "username", "domain"}
                ids.append(response.json()["id"])
            assert db.get(Credential, uuid.UUID(ids[0])).user_id == owner.id
            assert all(
                value not in client.get("/api/credentials").text
                for value in ("top-secret", "ciphertext", "nonce", "key_version")
            )
            other = as_user(client, db)
            assert client.get("/api/credentials").json() == []
            for index, protocol, credential in [(0, "rdp", ids[0]), (2, "ssh", ids[1])]:
                body = (
                    {"width": 1280, "height": 720}
                    if protocol == "rdp"
                    else {"columns": 80, "rows": 24}
                )
                path = f"/api/devices/{devices[index].id}"
                assert (
                    client.post(
                        f"{path}/{protocol}-sessions",
                        headers=write_headers(),
                        json={**body, "credential_id": credential},
                    ).status_code
                    == 400
                )
                assert (
                    client.put(
                        f"{path}/quick-connect-preferences",
                        headers=write_headers(),
                        json={"protocol": protocol, "credential_id": credential},
                    ).status_code
                    == 400
                )
            assert other.id != owner.id
            switch_user(client, owner)
            for index, device in enumerate(devices):
                protocol = "rdp" if index < 2 else "ssh"
                credential = ids[0 if index < 2 else 1]
                body = (
                    {"width": 1280, "height": 720}
                    if protocol == "rdp"
                    else {"columns": 80, "rows": 24}
                )
                path = f"/api/devices/{device.id}"
                assert (
                    client.post(
                        f"{path}/{protocol}-sessions",
                        headers=write_headers(),
                        json={**body, "credential_id": credential},
                    ).status_code
                    == 201
                )
                assert (
                    client.put(
                        f"{path}/quick-connect-preferences",
                        headers=write_headers(),
                        json={"protocol": protocol, "credential_id": credential},
                    ).status_code
                    == 200
                )
                wrong = ids[1 if index < 2 else 0]
                assert (
                    client.post(
                        f"{path}/{protocol}-sessions",
                        headers=write_headers(),
                        json={**body, "credential_id": wrong},
                    ).status_code
                    == 400
                )
            db.delete(devices[0])
            db.commit()
            assert db.get(Credential, uuid.UUID(ids[0])) is not None
    finally:
        app.dependency_overrides.clear()


def test_legacy_migration_preserves_history(tmp_path):
    import os
    import subprocess
    import sys
    from datetime import UTC, datetime
    from pathlib import Path

    from sqlalchemy import create_engine, text

    backend = Path(__file__).resolve().parents[1]
    url = f"sqlite:///{tmp_path / 'old.sqlite'}"
    env = {**os.environ, "DATABASE_URL": url}

    def upgrade(revision):
        subprocess.run(
            [sys.executable, "-m", "alembic", "upgrade", revision],
            cwd=backend,
            env=env,
            check=True,
            capture_output=True,
        )

    upgrade("0006")
    engine = create_engine(url)
    user, device, cred, session, audit = [uuid.uuid4().hex for _ in range(5)]
    now = datetime.now(UTC).isoformat()
    with engine.begin() as db:
        db.execute(
            text(
                "INSERT INTO users (id,oidc_issuer,oidc_subject,email,display_name,role,created_at) VALUES (:u,'i','s','a@b.com','A','admin',:t)"
            ),
            {"u": user, "t": now},
        )
        db.execute(
            text(
                "INSERT INTO devices (id,device_uuid,hostname,os_family,os_version,architecture,agent_version,capabilities,addresses,online,enrolled_at,created_at,updated_at) VALUES (:d,:du,'host','linux','','','','[]','[]',0,:t,:t,:t)"
            ),
            {"d": device, "du": uuid.uuid4().hex, "t": now},
        )
        db.execute(
            text(
                "INSERT INTO credentials (id,device_id,label,kind,username,ciphertext,nonce,key_version,created_at,updated_at) VALUES (:c,:d,'old','linux_password','root',:secret,:nonce,1,:t,:t)"
            ),
            {"c": cred, "d": device, "secret": b"oldsecret", "nonce": b"oldnonce", "t": now},
        )
        db.execute(
            text(
                "INSERT INTO remote_sessions (id,device_id,user_id,credential_id,state,columns,rows,created_at,last_activity_at,protocol,dpi) VALUES (:s,:d,:u,:c,'closed',80,24,:t,:t,'ssh',96)"
            ),
            {"s": session, "d": device, "u": user, "c": cred, "t": now},
        )
        db.execute(
            text(
                "INSERT INTO quick_connect_preferences (user_id,device_id,protocol,credential_id,preferred) VALUES (:u,:d,'ssh',:c,1)"
            ),
            {"u": user, "d": device, "c": cred},
        )
        db.execute(
            text(
                "INSERT INTO audit_events (id,event_type,actor_user_id,detail,created_at) VALUES (:a,'test',:u,'{}',:t)"
            ),
            {"a": audit, "u": user, "t": now},
        )
    upgrade("head")
    with engine.connect() as db:
        assert db.scalar(text("SELECT count(*) FROM credentials")) == 0
        assert db.scalar(text("SELECT count(*) FROM quick_connect_preferences")) == 0
        assert (
            db.scalar(
                text("SELECT credential_id FROM remote_sessions WHERE id=:id"), {"id": session}
            )
            is None
        )
        assert db.scalar(text("SELECT count(*) FROM audit_events WHERE id=:id"), {"id": audit}) == 1
