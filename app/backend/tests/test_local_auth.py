import logging

import pytest
from fastapi import HTTPException
from fastapi.testclient import TestClient
from pydantic import ValidationError
from sqlalchemy import select

from jump.config import Settings
from jump.db import get_db
from jump.local_auth import authenticate_local, hasher
from jump.main import app, cfg
from jump.models import AuditEvent, LocalSetup, Role, User
from jump.security import map_oidc_user

ORIGIN = "http://localhost:8000"


@pytest.fixture
def client(db):
    app.dependency_overrides[get_db] = lambda: db
    with TestClient(app, base_url=ORIGIN) as c:
        yield c
    app.dependency_overrides.clear()


def headers(csrf):
    return {"Origin": ORIGIN, "X-CSRF-Token": csrf}


def test_modes_and_validation(monkeypatch, client, db):
    with pytest.raises(ValidationError):
        Settings(auth_mode="invalid")
    for mode, oidc, local in [
        ("oidc", True, False),
        ("local", False, True),
        ("hybrid", True, True),
    ]:
        monkeypatch.setattr(cfg, "auth_mode", mode)
        result = client.get("/api/auth/config")
        assert result.status_code == 200
        assert result.json()["oidc_enabled"] is oidc
        assert result.json()["local_enabled"] is local
        assert bool(result.json()["csrf"])
        assert result.json()["setup_required"] is local
        if not local:
            assert (
                client.post(
                    "/api/auth/local/login",
                    json={"username": "admin", "password": "secret"},
                    headers=headers(result.json()["csrf"]),
                ).status_code
                == 404
            )
        if not oidc:
            assert client.get("/auth/login").status_code == 404
            assert client.get("/auth/callback").status_code == 404
    for mode in ("oidc", "hybrid"):
        with pytest.raises(RuntimeError, match="OIDC_ISSUER"):
            Settings(auth_mode=mode, oidc_issuer="").validate_production()
    Settings(
        auth_mode="local",
        oidc_issuer="",
        oidc_client_id="",
        oidc_client_secret="",
        oidc_redirect_uri="",
    ).validate_production()


def test_bootstrap_and_login(monkeypatch, client, db, caplog):
    monkeypatch.setattr(cfg, "auth_mode", "local")
    with caplog.at_level(logging.WARNING, logger="jump"):
        config = client.get("/api/auth/config").json()
    token = caplog.records[-1].message.split(": ")[-1]
    assert token not in str(db.get(LocalSetup, 1).token_hash)
    payload = {
        "token": token,
        "username": "Admin",
        "display_name": "Administrator",
        "password": "a very long passphrase",
        "password_confirmation": "a very long passphrase",
    }
    url = "/api/auth/local/setup"
    assert client.post(url, json=payload, headers=headers("wrong")).status_code == 403
    assert (
        client.post(
            url, json={**payload, "token": "invalid"}, headers=headers(config["csrf"])
        ).status_code
        == 403
    )
    response = client.post(url, json=payload, headers=headers(config["csrf"]))
    assert response.status_code == 200
    user = db.scalar(select(User).where(User.local_username == "admin"))
    assert user.role == Role.ADMIN and user.oidc_issuer is None and user.oidc_subject is None
    assert user.password_hash != payload["password"] and user.password_hash.startswith("$argon2id$")
    assert hasher.verify(user.password_hash, payload["password"])
    assert client.get("/api/me").json()["id"] == str(user.id)
    assert db.get(LocalSetup, 1) is None
    assert (
        client.post(
            url, json=payload, headers=headers(client.get("/api/me").json()["csrf"])
        ).status_code
        == 403
    )
    assert (
        client.post(
            "/api/logout", headers=headers(client.get("/api/me").json()["csrf"])
        ).status_code
        == 200
    )
    preauth = client.get("/api/auth/config").json()["csrf"]
    assert (
        client.post(
            "/api/auth/local/login", json={"username": "admin", "password": payload["password"]}
        ).status_code
        == 403
    )
    login = client.post(
        "/api/auth/local/login",
        json={"username": "ADMIN", "password": payload["password"]},
        headers=headers(preauth),
    )
    assert login.status_code == 200
    assert client.get("/api/me").json()["id"] == str(user.id)
    assert db.scalar(
        select(AuditEvent).where(
            AuditEvent.event_type == "user_login", AuditEvent.actor_user_id == user.id
        )
    )
    assert user.last_login_at is not None


def test_generic_failure_and_lockout(db):
    user = User(
        local_username="admin",
        password_hash=hasher.hash("correct password"),
        email="",
        display_name="Admin",
        role=Role.ADMIN,
    )
    db.add(user)
    db.commit()
    errors = []
    for username, password in [("admin", "incorrect"), ("nobody", "incorrect")]:
        with pytest.raises(HTTPException) as caught:
            authenticate_local(db, username, password)
        errors.append((caught.value.status_code, caught.value.detail))
    assert errors[0] == errors[1] == (401, "Invalid username or password")
    for _ in range(4):
        with pytest.raises(HTTPException):
            authenticate_local(db, "admin", "incorrect")
    with pytest.raises(HTTPException) as locked:
        authenticate_local(db, "admin", "correct password")
    assert locked.value.status_code == 401


def test_existing_oidc_identity_blocks_bootstrap(monkeypatch, client, db):
    monkeypatch.setattr(cfg, "auth_mode", "hybrid")
    original = map_oidc_user(db, "issuer", {"sub": "existing", "name": "Existing"})
    assert map_oidc_user(db, "issuer", {"sub": "existing", "name": "Renamed"}).id == original.id
    assert client.get("/api/auth/config").json()["setup_required"] is False
    assert db.scalar(select(LocalSetup)) is None
    csrf = client.get("/api/auth/config").json()["csrf"]
    assert (
        client.post(
            "/api/auth/local/setup",
            json={
                "token": "anything",
                "username": "admin",
                "display_name": "Admin",
                "password": "long password here",
                "password_confirmation": "long password here",
            },
            headers=headers(csrf),
        ).status_code
        == 403
    )
