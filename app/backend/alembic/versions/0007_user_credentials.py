"""Replace legacy device credentials with private, reusable user credentials.

Revision ID: 0007
Revises: 0006

Legacy secrets and their Quick Connect preferences are intentionally discarded.
Session and audit history remains; old session credential references become NULL.
"""

import sqlalchemy as sa

from alembic import op

revision = "0007"
down_revision = "0006"
branch_labels = None
depends_on = None


def _session_fk():
    return (
        "fk_remote_sessions_credential_id_credentials"
        if op.get_bind().dialect.name == "sqlite"
        else "remote_sessions_credential_id_fkey"
    )


def upgrade():
    # Remove dependent references before dropping the legacy secret table.
    op.drop_table("quick_connect_preferences")
    with op.batch_alter_table(
        "remote_sessions",
        naming_convention={"fk": "fk_%(table_name)s_%(column_0_name)s_%(referred_table_name)s"},
    ) as batch:
        batch.drop_constraint(_session_fk(), type_="foreignkey")
        batch.alter_column("credential_id", existing_type=sa.Uuid(), nullable=True)
    op.execute(sa.text("UPDATE remote_sessions SET credential_id = NULL"))
    op.drop_index("ix_credentials_device", table_name="credentials")
    op.drop_table("credentials")
    op.create_table(
        "credentials",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column(
            "user_id", sa.Uuid(), sa.ForeignKey("users.id", ondelete="CASCADE"), nullable=False
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
    op.create_index("ix_credentials_user", "credentials", ["user_id"])
    with op.batch_alter_table("remote_sessions") as batch:
        batch.create_foreign_key(
            "remote_sessions_credential_id_fkey",
            "credentials",
            ["credential_id"],
            ["id"],
            ondelete="SET NULL",
        )
    op.create_table(
        "quick_connect_preferences",
        sa.Column(
            "user_id", sa.Uuid(), sa.ForeignKey("users.id", ondelete="CASCADE"), primary_key=True
        ),
        sa.Column(
            "device_id",
            sa.Uuid(),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            primary_key=True,
        ),
        sa.Column("protocol", sa.String(16), primary_key=True),
        sa.Column(
            "credential_id",
            sa.Uuid(),
            sa.ForeignKey("credentials.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("preferred", sa.Boolean(), nullable=False, server_default=sa.false()),
    )


def downgrade():
    # The old secrets cannot be restored; keep history while reverting the schema.
    op.drop_table("quick_connect_preferences")
    with op.batch_alter_table("remote_sessions") as batch:
        batch.drop_constraint("remote_sessions_credential_id_fkey", type_="foreignkey")
    op.drop_index("ix_credentials_user", table_name="credentials")
    op.drop_table("credentials")
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
    with op.batch_alter_table("remote_sessions") as batch:
        batch.create_foreign_key(
            "remote_sessions_credential_id_fkey",
            "credentials",
            ["credential_id"],
            ["id"],
            ondelete="CASCADE",
        )
    op.create_table(
        "quick_connect_preferences",
        sa.Column(
            "user_id", sa.Uuid(), sa.ForeignKey("users.id", ondelete="CASCADE"), primary_key=True
        ),
        sa.Column(
            "device_id",
            sa.Uuid(),
            sa.ForeignKey("devices.id", ondelete="CASCADE"),
            primary_key=True,
        ),
        sa.Column("protocol", sa.String(16), primary_key=True),
        sa.Column(
            "credential_id",
            sa.Uuid(),
            sa.ForeignKey("credentials.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("preferred", sa.Boolean(), nullable=False, server_default=sa.false()),
    )
