import base64
import hashlib
import secrets
from datetime import timedelta

from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from fastapi import HTTPException
from sqlalchemy import select, update
from sqlalchemy.orm import Session

from .config import settings
from .models import AuditEvent, EnrollmentToken, Role, User, now


def hash_token(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


def create_enrollment(db: Session, user: User, os_family: str) -> tuple[str, EnrollmentToken]:
    token = secrets.token_urlsafe(32)
    record = EnrollmentToken(
        token_hash=hash_token(token),
        created_by_id=user.id,
        intended_os=os_family,
        expires_at=now() + timedelta(minutes=15),
    )
    db.add(record)
    db.add(AuditEvent(event_type="enrollment_token_created", actor_user_id=user.id))
    db.commit()
    return token, record


def consume_enrollment(db: Session, token: str, os_family: str) -> EnrollmentToken:
    # An atomic UPDATE ensures competing enrollments cannot redeem the same token.
    record = db.scalar(
        update(EnrollmentToken)
        .where(
            EnrollmentToken.token_hash == hash_token(token),
            EnrollmentToken.used_at.is_(None),
            EnrollmentToken.revoked_at.is_(None),
            EnrollmentToken.expires_at > now(),
            EnrollmentToken.intended_os == os_family,
        )
        .values(used_at=now())
        .returning(EnrollmentToken)
    )
    if record is None:
        raise HTTPException(400, "Invalid or expired enrollment token")
    return record


def map_oidc_user(db: Session, issuer: str, claims: dict) -> User:
    subject = claims.get("sub")
    if not isinstance(subject, str) or not subject:
        raise ValueError("OIDC subject is missing")
    email = claims.get("email") or claims.get("preferred_username") or ""
    user = db.scalar(select(User).where(User.oidc_issuer == issuer, User.oidc_subject == subject))
    if user is None:
        # Lock the users table to serialize the first-admin decision across workers.
        if db.bind and db.bind.dialect.name == "postgresql":
            db.connection().exec_driver_sql("LOCK TABLE users IN EXCLUSIVE MODE")
        user = db.scalar(
            select(User).where(User.oidc_issuer == issuer, User.oidc_subject == subject)
        )
        if user is None:
            first = db.scalar(select(User.id).limit(1)) is None
            user = User(
                oidc_issuer=issuer,
                oidc_subject=subject,
                email=email,
                display_name=claims.get("name") or email or subject,
                role=Role.ADMIN if first else Role.USER,
            )
            db.add(user)
    user.email = email
    user.display_name = claims.get("name") or email or subject
    user.last_login_at = now()
    db.flush()
    db.add(AuditEvent(event_type="user_login", actor_user_id=user.id))
    db.commit()
    return user


def master_key() -> bytes:
    try:
        key = base64.b64decode(settings().jump_master_key, validate=True)
    except ValueError as exc:
        raise RuntimeError("JUMP_MASTER_KEY must be base64 encoded") from exc
    if len(key) != 32:
        raise RuntimeError("JUMP_MASTER_KEY must encode exactly 32 bytes")
    return key


def encrypt_secret(secret: bytes, credential_id: str, key: bytes) -> tuple[bytes, bytes, int]:
    nonce = secrets.token_bytes(12)
    return AESGCM(key).encrypt(nonce, secret, credential_id.encode()), nonce, 1


def decrypt_secret(ciphertext: bytes, nonce: bytes, credential_id: str, key: bytes) -> bytes:
    return AESGCM(key).decrypt(nonce, ciphertext, credential_id.encode())


def require_admin(user: User) -> None:
    if user.role != Role.ADMIN:
        raise HTTPException(403, "Admin role required")
