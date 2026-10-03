"""Allow Screen gateways to observe an authenticated termination request."""

import sqlalchemy as sa

from alembic import op

revision = "0012"
down_revision = "0011"
branch_labels = None
depends_on = None


def upgrade():
    op.add_column(
        "remote_sessions",
        sa.Column("screen_close_requested_at", sa.DateTime(timezone=True), nullable=True),
    )


def downgrade():
    op.drop_column("remote_sessions", "screen_close_requested_at")
