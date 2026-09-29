"""Persist safe remote session lifecycle stages and creation request correlation."""

import sqlalchemy as sa

from alembic import op

revision = "0008"
down_revision = "0007"
branch_labels = None
depends_on = None


def upgrade():
    op.add_column("remote_sessions", sa.Column("request_id", sa.String(36), nullable=True))
    op.create_table(
        "remote_session_diagnostic_events",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column(
            "session_id",
            sa.Uuid(),
            sa.ForeignKey("remote_sessions.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("stage", sa.String(40), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.UniqueConstraint("session_id", "stage", name="uq_remote_session_diagnostic_stage"),
    )
    op.create_index(
        "ix_remote_session_diagnostic_session", "remote_session_diagnostic_events", ["session_id"]
    )


def downgrade():
    op.drop_index(
        "ix_remote_session_diagnostic_session", table_name="remote_session_diagnostic_events"
    )
    op.drop_table("remote_session_diagnostic_events")
    op.drop_column("remote_sessions", "request_id")
