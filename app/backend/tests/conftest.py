import os

os.environ.setdefault("SESSION_SECRET", "test-session-secret-at-least-32-characters")
os.environ.setdefault("BROKER_INTERNAL_TOKEN", "test-broker-token-at-least-32-characters")
os.environ.setdefault("JUMP_MASTER_KEY", "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY=")
os.environ.setdefault("OIDC_ISSUER", "https://issuer.example")
os.environ.setdefault("OIDC_CLIENT_ID", "test-client")
os.environ.setdefault("OIDC_CLIENT_SECRET", "test-secret")
os.environ.setdefault("OIDC_REDIRECT_URI", "http://localhost:8000/auth/callback")
os.environ.setdefault("PUBLIC_URL", "http://localhost:8000")
os.environ.setdefault("COOKIE_SECURE", "false")

import pytest
from sqlalchemy import create_engine
from sqlalchemy.orm import Session
from sqlalchemy.pool import StaticPool

from jump import models  # noqa: F401
from jump.db import Base


@pytest.fixture
def db():
    engine = create_engine(
        "sqlite://", connect_args={"check_same_thread": False}, poolclass=StaticPool
    )
    Base.metadata.create_all(engine)
    with Session(engine, expire_on_commit=False) as session:
        yield session
    engine.dispose()
