"""Small local identity boundary; downstream authorization uses User.id only."""

import hmac
import logging
import re
import secrets
from datetime import timedelta

from argon2 import PasswordHasher
from argon2.exceptions import VerificationError, VerifyMismatchError
from fastapi import HTTPException
from sqlalchemy import select
from sqlalchemy.orm import Session

from .models import AuditEvent, LocalLoginFailure, LocalSetup, Role, User, now
from .security import hash_token

log = logging.getLogger("jump")
hasher = PasswordHasher(time_cost=3, memory_cost=65536, parallelism=4)
_dummy_hash = hasher.hash("dummy-password-for-constant-work")


def normalize_username(value: str) -> str:
    normalized = value.strip().lower()
    if not re.fullmatch(r"[a-z0-9][a-z0-9_.-]{2,99}", normalized):
        raise HTTPException(
            400, "Username must be 3–100 letters, numbers, dots, dashes, or underscores"
        )
    return normalized


def validate_password(value: str) -> None:
    if not 12 <= len(value) <= 1024 or len(value.encode("utf-8")) > 4096:
        raise HTTPException(400, "Password must be 12–1024 characters")


def lock_users(db: Session) -> None:
    if db.bind and db.bind.dialect.name == "postgresql":
        db.connection().exec_driver_sql("LOCK TABLE users IN EXCLUSIVE MODE")


def setup_required(db: Session) -> bool:
    return db.scalar(select(User.id).limit(1)) is None


def ensure_setup_token(db: Session) -> None:
    """Issue once after first login-page visit, under the same lock as first-admin creation."""
    lock_users(db)
    if not setup_required(db) or db.get(LocalSetup, 1):
        return
    token = secrets.token_urlsafe(32)
    db.add(LocalSetup(id=1, token_hash=hash_token(token)))
    db.commit()
    log.warning("Initial local administrator setup token (shown once): %s", token)


def create_first_admin(
    db: Session, token: str, username: str, display_name: str, password: str, confirmation: str
) -> User:
    if len(token) > 256 or not token or not hmac.compare_digest(password, confirmation):
        raise HTTPException(400, "Invalid setup details")
    normalized = normalize_username(username)
    validate_password(password)
    name = display_name.strip()
    if not 1 <= len(name) <= 255:
        raise HTTPException(400, "Display name is required (up to 255 characters)")
    lock_users(db)
    record = db.get(LocalSetup, 1)
    if (
        not setup_required(db)
        or record is None
        or not hmac.compare_digest(record.token_hash, hash_token(token))
    ):
        raise HTTPException(403, "Invalid or unavailable setup token")
    user = User(
        local_username=normalized,
        password_hash=hasher.hash(password),
        email="",
        display_name=name,
        role=Role.ADMIN,
    )
    db.add(user)
    db.flush()
    db.delete(record)
    user.last_login_at = now()
    db.add(AuditEvent(event_type="user_login", actor_user_id=user.id))
    db.commit()
    return user


def authenticate_local(db: Session, username: str, password: str) -> User:
    # Use the same response and hash work for missing and incorrect usernames.
    failure = HTTPException(401, "Invalid username or password")
    normalized = username.strip().lower()
    if len(normalized) > 100 or len(password) > 1024 or len(password.encode("utf-8")) > 4096:
        raise failure
    if db.bind and db.bind.dialect.name == "postgresql":
        # Transaction-scoped lock serializes failed attempts for a given username.
        db.connection().exec_driver_sql("SELECT pg_advisory_xact_lock(hashtext(%s))", (normalized,))
    attempts = db.get(LocalLoginFailure, normalized)
    locked = bool(attempts and attempts.locked_until and attempts.locked_until > now())
    user = db.scalar(select(User).where(User.local_username == normalized))
    try:
        verified = hasher.verify(
            user.password_hash if user and user.password_hash else _dummy_hash, password
        )
    except (VerifyMismatchError, VerificationError):
        verified = False
    if locked or not user or not verified:
        # Track only real accounts so random usernames cannot fill the table.
        if user and not locked:
            if not attempts:
                attempts = LocalLoginFailure(username=normalized, attempts=0)
                db.add(attempts)
            attempts.attempts += 1
            if attempts.attempts >= 5:
                attempts.locked_until = now() + timedelta(minutes=15)
        db.commit()
        raise failure
    if attempts:
        db.delete(attempts)
    user.last_login_at = now()
    db.add(AuditEvent(event_type="user_login", actor_user_id=user.id))
    db.commit()
    return user
