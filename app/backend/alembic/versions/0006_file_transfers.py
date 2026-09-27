"""Agent file transfers.

Revision ID: 0006
Revises: 0005
"""

import sqlalchemy as sa

from alembic import op

revision = "0006"
down_revision = "0005"
branch_labels = None
depends_on = None


def upgrade():
    op.create_table(
        "file_transfers",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column(
            "user_id", sa.Uuid(), sa.ForeignKey("users.id", ondelete="CASCADE"), nullable=False
        ),
        sa.Column(
            "device_id", sa.Uuid(), sa.ForeignKey("devices.id", ondelete="CASCADE"), nullable=False
        ),
        sa.Column("connection_id", sa.String(36), nullable=False),
        sa.Column("direction", sa.String(16), nullable=False),
        sa.Column("remote_path", sa.String(4096), nullable=False),
        sa.Column("filename", sa.String(255), nullable=False),
        sa.Column("expected_size", sa.BigInteger(), nullable=True),
        sa.Column("transferred_bytes", sa.BigInteger(), nullable=False, server_default="0"),
        sa.Column("state", sa.String(16), nullable=False),
        sa.Column("sha256", sa.String(64), nullable=True),
        sa.Column("failure_reason", sa.String(64), nullable=True),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("last_activity_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("completed_at", sa.DateTime(timezone=True), nullable=True),
    )
    op.create_index("ix_file_transfers_device", "file_transfers", ["device_id"])


def downgrade():
    op.drop_index("ix_file_transfers_device", table_name="file_transfers")
    op.drop_table("file_transfers")
