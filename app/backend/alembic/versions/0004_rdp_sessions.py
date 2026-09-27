"""Distinguish SSH and RDP remote sessions.

Revision ID: 0004
Revises: 0003
"""

import sqlalchemy as sa

from alembic import op

revision = "0004"
down_revision = "0003"
branch_labels = None
depends_on = None


def upgrade():
    op.add_column(
        "remote_sessions",
        sa.Column("protocol", sa.String(16), nullable=False, server_default="ssh"),
    )
    op.add_column(
        "remote_sessions", sa.Column("dpi", sa.Integer(), nullable=False, server_default="96")
    )


def downgrade():
    op.drop_column("remote_sessions", "dpi")
    op.drop_column("remote_sessions", "protocol")
