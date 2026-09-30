"""Bind console sessions to a connection and reserve one controller per device."""

import sqlalchemy as sa

from alembic import op

revision = "0011"
down_revision = "0010"
branch_labels = None
depends_on = None


def upgrade():
    op.add_column("remote_sessions", sa.Column("connection_id", sa.String(36), nullable=True))
    op.create_index(
        "uq_screen_device_active",
        "remote_sessions",
        ["device_id"],
        unique=True,
        postgresql_where=sa.text("protocol = 'screen' AND state IN ('connecting', 'active')"),
        sqlite_where=sa.text("protocol = 'screen' AND state IN ('connecting', 'active')"),
    )


def downgrade():
    op.drop_index("uq_screen_device_active", table_name="remote_sessions")
    op.drop_column("remote_sessions", "connection_id")
