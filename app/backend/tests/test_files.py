import base64
import hashlib
import json
import uuid

import pytest
from fastapi.testclient import TestClient
from test_api import as_user, write_headers

from jump import main
from jump.db import get_db
from jump.models import AgentIdentity, AuditEvent, Device, Role


@pytest.fixture
def client(db):
    main.app.dependency_overrides[get_db] = lambda: db
    with TestClient(main.app, base_url="http://localhost:8000") as test_client:
        yield test_client
    main.app.dependency_overrides.clear()


def device(db, capable=True):
    device = Device(
        hostname="test",
        os_family="linux",
        os_version="test",
        architecture="amd64",
        capabilities=["file_transfer_v1"] if capable else ["ssh"],
        online=True,
        connection_id=str(uuid.uuid4()),
    )
    db.add(device)
    db.flush()
    db.add(AgentIdentity(device_id=device.id, public_key=b"1" * 32))
    db.commit()
    return device


def test_admin_presence_capability_and_identity(client, db):
    target = device(db)
    url = f"/api/devices/{target.id}/files?path=%2F"
    assert client.get(url).status_code == 401
    user = as_user(client, db, Role.USER)
    assert client.get(url).status_code == 403
    user.role = Role.ADMIN
    db.commit()
    assert client.get(f"/api/devices/{uuid.uuid4()}/files").status_code == 404
    target.capabilities = ["ssh"]
    db.commit()
    assert client.get(url).json()["detail"] == "unsupported_agent"
    target.capabilities = ["file_transfer_v1"]
    target.agent_identity.revoked_at = main.now()
    db.commit()
    assert client.get(url).json()["detail"] == "device_offline"


class Socket:
    def __init__(self, transfer, body=b"", listing=False):
        self.id = str(transfer.id)
        self.body = body
        self.listing = listing
        self.received = bytearray()
        self.sent = []
        self.closed = False

    async def send(self, value):
        self.sent.append(json.loads(value))

    async def recv(self):
        message = self.sent[-1]
        typ = message["type"]
        if typ == "file_list":
            return json.dumps(
                {
                    "version": 1,
                    "transfer_id": self.id,
                    "type": "file_list_result",
                    "entries": [{"name": "log", "type": "file", "size": 3, "modified_at": ""}],
                }
            )
        if typ == "file_upload_open":
            return json.dumps({"version": 1, "transfer_id": self.id, "type": "file_opened"})
        if typ == "file_chunk":
            self.received.extend(base64.b64decode(message["data"]))
            return json.dumps(
                {
                    "version": 1,
                    "transfer_id": self.id,
                    "type": "file_opened",
                    "size": len(self.received),
                }
            )
        if typ == "file_finish":
            return json.dumps(
                {
                    "version": 1,
                    "transfer_id": self.id,
                    "type": "file_finished",
                    "size": len(self.received),
                    "sha256": hashlib.sha256(self.received).hexdigest(),
                }
            )
        raise AssertionError(typ)

    async def close(self):
        self.closed = True


def test_listing_and_streamed_upload_audit(client, db, monkeypatch):
    as_user(client, db)
    target = device(db)
    sockets = []

    async def socket(transfer):
        result = Socket(transfer)
        sockets.append(result)
        return result

    monkeypatch.setattr(main, "file_socket", socket)
    listed = client.get(f"/api/devices/{target.id}/files", params={"path": "/"})
    assert listed.status_code == 200, listed.text
    assert listed.json()["entries"][0]["name"] == "log"
    payload = b"secret contents" * 4000
    uploaded = client.post(
        f"/api/devices/{target.id}/files/upload",
        params={"path": "/tmp", "filename": "secret.txt", "size": len(payload)},
        content=payload,
        headers={**write_headers(), "Content-Type": "application/octet-stream"},
    )
    assert uploaded.status_code == 200, uploaded.text
    assert sockets[-1].received == payload
    assert len([m for m in sockets[-1].sent if m["type"] == "file_chunk"]) > 1
    assert uploaded.json()["sha256"] == hashlib.sha256(payload).hexdigest()
    events = db.query(AuditEvent).filter(AuditEvent.event_type == "file_upload_completed").all()
    assert len(events) == 1
    assert payload.decode() not in json.dumps(events[0].detail)
    assert all(item.closed for item in sockets)


def test_transfer_claim_and_owner_scope(client, db):
    user = as_user(client, db)
    target = device(db)
    transfer = main.file_record(db, user, target, "upload", "/tmp", "data", 12)
    authorized = client.get(
        f"/api/internal/file-transfers/{transfer.id}/authorize",
        params={
            "device_id": str(target.id),
            "connection_id": target.connection_id,
            "user_id": str(user.id),
        },
        headers={"Authorization": "Bearer test-broker-token-at-least-32-characters"},
    )
    assert authorized.status_code == 200
    wrong = client.get(
        f"/api/internal/file-transfers/{transfer.id}/authorize",
        params={
            "device_id": str(target.id),
            "connection_id": target.connection_id,
            "user_id": str(uuid.uuid4()),
        },
        headers={"Authorization": "Bearer test-broker-token-at-least-32-characters"},
    )
    assert wrong.status_code == 403
    assert client.get(f"/api/devices/{target.id}/file-transfers").json()[0]["id"] == str(
        transfer.id
    )
    as_user(client, db)
    assert (
        client.post(
            f"/api/file-transfers/{transfer.id}/cancel", headers=write_headers()
        ).status_code
        == 404
    )
    assert client.get(f"/api/devices/{target.id}/file-transfers").json() == []
    transfer.state = "completed"
    db.commit()
    assert authorized.status_code == 200
    assert (
        client.get(
            f"/api/internal/file-transfers/{transfer.id}/authorize",
            params={
                "device_id": str(target.id),
                "connection_id": target.connection_id,
                "user_id": str(user.id),
            },
            headers={"Authorization": "Bearer test-broker-token-at-least-32-characters"},
        ).status_code
        == 403
    )


def test_download_stream_and_safe_header(client, db, monkeypatch):
    as_user(client, db)
    target = device(db)
    payload = b"private-download-bytes" * 2500
    sockets = []

    class DownloadSocket(Socket):
        def __init__(self, transfer):
            super().__init__(transfer)
            self.step = 0

        async def recv(self):
            self.step += 1
            frame = {"version": 1, "transfer_id": self.id}
            if self.step == 1:
                frame.update(type="file_opened", size=len(payload))
            elif self.step in (2, 3):
                part = payload[
                    (self.step - 2) * main.FILE_CHUNK : (self.step - 1) * main.FILE_CHUNK
                ]
                frame.update(type="file_chunk", data=base64.b64encode(part).decode())
            else:
                frame.update(
                    type="file_finished",
                    size=len(payload),
                    sha256=hashlib.sha256(payload).hexdigest(),
                )
            return json.dumps(frame)

    async def socket(transfer):
        result = DownloadSocket(transfer)
        sockets.append(result)
        return result

    monkeypatch.setattr(main, "file_socket", socket)
    result = client.get(
        f"/api/devices/{target.id}/files/download", params={"path": r"/tmp/back\slash"}
    )
    assert result.status_code == 200, result.text
    assert result.content == payload
    assert result.headers["content-length"] == str(len(payload))
    assert "filename*=UTF-8''back%5Cslash" in result.headers["content-disposition"]
    assert len([frame for frame in sockets[0].sent if frame["type"] == "file_ack"]) == 2
    assert sockets[0].closed
    assert (
        db.query(AuditEvent).filter(AuditEvent.event_type == "file_download_completed").count() == 1
    )


def test_directory_claim_is_ephemeral_on_success_and_failure(client, db, monkeypatch):
    from sqlalchemy import func, select

    from jump.models import FileTransfer

    as_user(client, db)
    target = device(db)
    observed = []
    failing = False

    class ListingSocket(Socket):
        async def recv(self):
            if failing:
                return json.dumps(
                    {
                        "version": 1,
                        "transfer_id": self.id,
                        "type": "file_error",
                        "code": "permission_denied",
                    }
                )
            return await super().recv()

    async def socket(transfer):
        observed.append(
            db.scalar(
                select(func.count())
                .select_from(FileTransfer)
                .where(FileTransfer.direction == "list")
            )
        )
        return ListingSocket(transfer)

    monkeypatch.setattr(main, "file_socket", socket)
    url = f"/api/devices/{target.id}/files"
    for _ in range(4):
        assert client.get(url, params={"path": "/tmp"}).status_code == 200
        assert db.scalar(select(func.count()).select_from(FileTransfer)) == 0
    failing = True
    assert client.get(url, params={"path": "/tmp"}).json()["detail"] == "permission_denied"
    assert db.scalar(select(func.count()).select_from(FileTransfer)) == 0
    assert observed == [1] * 5


def test_directory_claim_removed_when_request_is_cancelled(client, db, monkeypatch):
    import asyncio

    from sqlalchemy import func, select

    from jump.models import FileTransfer

    user = as_user(client, db)
    target = device(db)

    async def cancelled_socket(transfer):
        assert db.get(FileTransfer, transfer.id) is not None
        raise asyncio.CancelledError()

    monkeypatch.setattr(main, "file_socket", cancelled_socket)
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(main.list_files(target.id, "/", 0, user, db))
    assert db.scalar(select(func.count()).select_from(FileTransfer)) == 0


def test_cancel_owner_audit_and_idempotence(client, db, monkeypatch):
    from jump.models import FileTransfer

    owner = as_user(client, db)
    target = device(db)
    transfer = main.file_record(db, owner, target, "upload", "/tmp", "secret.bin", 999)
    other = as_user(client, db)
    cancel_url = f"/api/file-transfers/{transfer.id}/cancel"
    assert other.id != owner.id
    assert client.post(cancel_url, headers=write_headers()).status_code == 404
    assert db.get(FileTransfer, transfer.id).state == "pending"
    # Restore the original signed browser session for the owner.
    from itsdangerous import TimestampSigner

    state = base64.b64encode(json.dumps({"uid": str(owner.id), "csrf": "test-csrf"}).encode())
    client.cookies.set(
        "jump_session",
        TimestampSigner("test-session-secret-at-least-32-characters").sign(state).decode(),
    )
    calls = []

    class BrokerClient:
        def __init__(self, *args, **kwargs):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            pass

        async def post(self, url, **kwargs):
            calls.append((url, kwargs))

    monkeypatch.setattr(main.httpx, "AsyncClient", BrokerClient)
    first = client.post(cancel_url, headers=write_headers())
    assert first.status_code == 200, first.text
    assert first.json()["state"] == "cancelled"
    assert first.json()["failure_reason"] == "transfer_cancelled"
    assert calls[0][0].endswith(f"/internal/file-streams/{transfer.id}/cancel")
    assert calls[0][1]["json"] == {"device_id": str(target.id), "user_id": str(owner.id)}
    assert client.post(cancel_url, headers=write_headers()).json()["state"] == "cancelled"
    assert len(calls) == 1
    audit = db.query(AuditEvent).filter(AuditEvent.event_type == "file_upload_cancelled").all()
    assert len(audit) == 1
    assert audit[0].detail["reason"] == "transfer_cancelled"
    assert "secret contents" not in json.dumps(audit[0].detail)
    assert "secret contents" not in json.dumps(main.file_output(transfer), default=str)
    completed = main.file_record(db, owner, target, "download", "/tmp/done", "done", 5)
    main.file_finish(db, completed, "completed", digest="a" * 64)
    assert (
        client.post(f"/api/file-transfers/{completed.id}/cancel", headers=write_headers()).json()[
            "state"
        ]
        == "completed"
    )
    assert len(calls) == 1


def test_transfer_size_columns_are_bigint():
    from sqlalchemy import BigInteger

    from jump.models import FileTransfer

    assert isinstance(FileTransfer.__table__.c.expected_size.type, BigInteger)
    assert isinstance(FileTransfer.__table__.c.transferred_bytes.type, BigInteger)
