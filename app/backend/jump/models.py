import uuid
from datetime import UTC, datetime
from enum import StrEnum

from sqlalchemy import (
    JSON,
    Boolean,
    Column,
    DateTime,
    ForeignKey,
    Index,
    Integer,
    LargeBinary,
    String,
    Table,
    UniqueConstraint,
)
from sqlalchemy.orm import Mapped, mapped_column, relationship

from .db import Base


def now() -> datetime:
    return datetime.now(UTC)


class Role(StrEnum):
    ADMIN = "admin"
    USER = "user"


device_tags = Table(
    "device_tags",
    Base.metadata,
    Column("device_id", ForeignKey("devices.id", ondelete="CASCADE"), primary_key=True),
    Column("tag_id", ForeignKey("tags.id", ondelete="CASCADE"), primary_key=True),
)


class User(Base):
    __tablename__ = "users"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    oidc_issuer: Mapped[str] = mapped_column(String(512))
    oidc_subject: Mapped[str] = mapped_column(String(512))
    email: Mapped[str] = mapped_column(String(320))
    display_name: Mapped[str] = mapped_column(String(255))
    role: Mapped[str] = mapped_column(String(20), default=Role.USER)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    last_login_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    __table_args__ = (UniqueConstraint("oidc_issuer", "oidc_subject"),)


class Group(Base):
    __tablename__ = "groups"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(100), unique=True)


class Tag(Base):
    __tablename__ = "tags"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    name: Mapped[str] = mapped_column(String(100), unique=True)


class Device(Base):
    __tablename__ = "devices"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    device_uuid: Mapped[uuid.UUID] = mapped_column(unique=True, default=uuid.uuid4)
    hostname: Mapped[str] = mapped_column(String(255))
    display_name: Mapped[str | None] = mapped_column(String(255))
    os_family: Mapped[str] = mapped_column(String(32))
    os_version: Mapped[str] = mapped_column(String(255), default="")
    architecture: Mapped[str] = mapped_column(String(32), default="")
    agent_version: Mapped[str] = mapped_column(String(64), default="")
    capabilities: Mapped[list] = mapped_column(JSON, default=list)
    addresses: Mapped[list] = mapped_column(JSON, default=list)
    primary_ip: Mapped[str | None] = mapped_column(String(45))
    current_user: Mapped[str | None] = mapped_column(String(255))
    group_id: Mapped[uuid.UUID | None] = mapped_column(ForeignKey("groups.id", ondelete="SET NULL"))
    group: Mapped[Group | None] = relationship()
    tags: Mapped[list[Tag]] = relationship(secondary=device_tags, lazy="selectin")
    agent_identity: Mapped["AgentIdentity | None"] = relationship(
        "AgentIdentity", uselist=False, cascade="all, delete-orphan", passive_deletes=True
    )
    credentials: Mapped[list["Credential"]] = relationship(
        "Credential", cascade="all, delete-orphan", passive_deletes=True
    )
    online: Mapped[bool] = mapped_column(Boolean, default=False)
    ssh_host_key: Mapped[str | None] = mapped_column(String(128))
    connection_id: Mapped[str | None] = mapped_column(String(36))
    last_seen_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    enrolled_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    updated_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now, onupdate=now)


class EnrollmentToken(Base):
    __tablename__ = "enrollment_tokens"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    token_hash: Mapped[str] = mapped_column(String(64), unique=True)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    expires_at: Mapped[datetime] = mapped_column(DateTime(timezone=True))
    used_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    revoked_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    created_by_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("users.id"))
    intended_os: Mapped[str | None] = mapped_column(String(32))
    intended_hostname: Mapped[str | None] = mapped_column(String(255))


class AgentIdentity(Base):
    __tablename__ = "agent_identities"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    device_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("devices.id", ondelete="CASCADE"), unique=True
    )
    public_key: Mapped[bytes] = mapped_column(LargeBinary, unique=True)
    algorithm: Mapped[str] = mapped_column(String(32), default="Ed25519")
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    revoked_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))


class Credential(Base):
    __tablename__ = "credentials"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))
    label: Mapped[str] = mapped_column(String(255))
    kind: Mapped[str] = mapped_column(String(32))
    username: Mapped[str] = mapped_column(String(255))
    domain: Mapped[str | None] = mapped_column(String(255))
    ciphertext: Mapped[bytes] = mapped_column(LargeBinary)
    nonce: Mapped[bytes] = mapped_column(LargeBinary)
    key_version: Mapped[int] = mapped_column(Integer, default=1)
    default_for: Mapped[str | None] = mapped_column(String(16))
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    updated_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now, onupdate=now)
    __table_args__ = (Index("ix_credentials_device", "device_id"),)


class RemoteSession(Base):
    __tablename__ = "remote_sessions"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))
    user_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("users.id", ondelete="CASCADE"))
    credential_id: Mapped[uuid.UUID] = mapped_column(
        ForeignKey("credentials.id", ondelete="CASCADE")
    )
    state: Mapped[str] = mapped_column(String(16), default="connecting")
    protocol: Mapped[str] = mapped_column(String(16), default="ssh")
    columns: Mapped[int] = mapped_column(Integer)
    rows: Mapped[int] = mapped_column(Integer)
    dpi: Mapped[int] = mapped_column(Integer, default=96)
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    connected_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    attached_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    closed_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    last_activity_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    failure_reason: Mapped[str | None] = mapped_column(String(64))
    __table_args__ = (Index("ix_remote_sessions_device", "device_id"),)


class AgentUpdate(Base):
    __tablename__ = "agent_updates"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    device_id: Mapped[uuid.UUID] = mapped_column(ForeignKey("devices.id", ondelete="CASCADE"))
    actor_user_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL")
    )
    from_version: Mapped[str] = mapped_column(String(64))
    target_version: Mapped[str] = mapped_column(String(64))
    state: Mapped[str] = mapped_column(String(16), default="pending")
    connection_id: Mapped[str] = mapped_column(String(36))
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    started_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    restarting_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    completed_at: Mapped[datetime | None] = mapped_column(DateTime(timezone=True))
    failure_reason: Mapped[str | None] = mapped_column(String(64))
    __table_args__ = (Index("ix_agent_updates_device", "device_id"),)


class AuditEvent(Base):
    __tablename__ = "audit_events"
    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    event_type: Mapped[str] = mapped_column(String(64))
    actor_user_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("users.id", ondelete="SET NULL")
    )
    device_id: Mapped[uuid.UUID | None] = mapped_column(
        ForeignKey("devices.id", ondelete="SET NULL")
    )
    detail: Mapped[dict] = mapped_column(JSON, default=dict)
    request_id: Mapped[str | None] = mapped_column(String(36))
    created_at: Mapped[datetime] = mapped_column(DateTime(timezone=True), default=now)
    __table_args__ = (Index("ix_audit_created_at", "created_at"),)
