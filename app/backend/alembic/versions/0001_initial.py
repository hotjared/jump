"""Initial schema.

Revision ID: 0001
Revises:
"""

import sqlalchemy as sa

from alembic import op

revision = "0001"
down_revision = None
branch_labels = None
depends_on = None


def upgrade():
    op.create_table(
        "users",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column("oidc_issuer", sa.String(512), nullable=False),
        sa.Column("oidc_subject", sa.String(512), nullable=False),
        sa.Column("email", sa.String(320), nullable=False),
        sa.Column("display_name", sa.String(255), nullable=False),
        sa.Column("role", sa.String(20), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("last_login_at", sa.DateTime(timezone=True)),
        sa.UniqueConstraint("oidc_issuer", "oidc_subject"),
    )
    op.create_table(
        "groups",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column("name", sa.String(100), nullable=False, unique=True),
    )
    op.create_table(
        "tags",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column("name", sa.String(100), nullable=False, unique=True),
    )
    op.create_table(
        "devices",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column("device_uuid", sa.Uuid(), nullable=False, unique=True),
        sa.Column("hostname", sa.String(255), nullable=False),
        sa.Column("display_name", sa.String(255)),
        sa.Column("os_family", sa.String(32), nullable=False),
        sa.Column("os_version", sa.String(255), nullable=False),
        sa.Column("architecture", sa.String(32), nullable=False),
        sa.Column("agent_version", sa.String(64), nullable=False),
        sa.Column("capabilities", sa.JSON(), nullable=False),
        sa.Column("addresses", sa.JSON(), nullable=False),
        sa.Column("primary_ip", sa.String(45)),
        sa.Column("current_user", sa.String(255)),
        sa.Column("group_id", sa.Uuid(), sa.ForeignKey("groups.id", ondelete="SET NULL")),
        sa.Column("online", sa.Boolean(), nullable=False),
        sa.Column("connection_id", sa.String(36)),
        sa.Column("last_seen_at", sa.DateTime(timezone=True)),
        sa.Column("enrolled_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("updated_at", sa.DateTime(timezone=True), nullable=False),
    )
    op.create_table(
        "device_tags",
        sa.Column(
            "device_id",
            sa.Uuid(),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            primary_key=True,
        ),
        sa.Column(
            "tag_id", sa.Uuid(), sa.ForeignKey("tags.id", ondelete="CASCADE"), primary_key=True
        ),
    )
    op.create_table(
        "enrollment_tokens",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column("token_hash", sa.String(64), nullable=False, unique=True),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("expires_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("used_at", sa.DateTime(timezone=True)),
        sa.Column("revoked_at", sa.DateTime(timezone=True)),
        sa.Column("created_by_id", sa.Uuid(), sa.ForeignKey("users.id"), nullable=False),
        sa.Column("intended_os", sa.String(32)),
        sa.Column("intended_hostname", sa.String(255)),
    )
    op.create_table(
        "agent_identities",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column(
            "device_id",
            sa.Uuid(),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            nullable=False,
            unique=True,
        ),
        sa.Column("public_key", sa.LargeBinary(), nullable=False, unique=True),
        sa.Column("algorithm", sa.String(32), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("revoked_at", sa.DateTime(timezone=True)),
    )
    op.create_table(
        "credentials",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column(
            "device_id", sa.Uuid(), sa.ForeignKey("devices.id", ondelete="CASCADE"), nullable=False
        ),
        sa.Column("label", sa.String(255), nullable=False),
        sa.Column("kind", sa.String(32), nullable=False),
        sa.Column("username", sa.String(255), nullable=False),
        sa.Column("domain", sa.String(255)),
        sa.Column("ciphertext", sa.LargeBinary(), nullable=False),
        sa.Column("nonce", sa.LargeBinary(), nullable=False),
        sa.Column("key_version", sa.Integer(), nullable=False),
        sa.Column("default_for", sa.String(16)),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("updated_at", sa.DateTime(timezone=True), nullable=False),
    )
    op.create_index("ix_credentials_device", "credentials", ["device_id"])
    op.create_table(
        "audit_events",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column("event_type", sa.String(64), nullable=False),
        sa.Column("actor_user_id", sa.Uuid(), sa.ForeignKey("users.id", ondelete="SET NULL")),
        sa.Column("device_id", sa.Uuid(), sa.ForeignKey("devices.id", ondelete="SET NULL")),
        sa.Column("detail", sa.JSON(), nullable=False),
        sa.Column("request_id", sa.String(36)),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
    )
    op.create_index("ix_audit_created_at", "audit_events", ["created_at"])


def downgrade():
    op.drop_table("audit_events")
    op.drop_table("credentials")
    op.drop_table("agent_identities")
    op.drop_table("enrollment_tokens")
    op.drop_table("device_tags")
    op.drop_table("devices")
    op.drop_table("tags")
    op.drop_table("groups")
    op.drop_table("users")
