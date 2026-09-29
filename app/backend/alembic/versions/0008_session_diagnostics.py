"""Persist safe remote session lifecycle stages and creation request correlation."""

import sqlalchemy as sa

from alembic import op

revision = "0008"
down_revision = "0007"
branch_labels = None
depends_on = None


def _device_fk():
    return (
        "fk_remote_sessions_device_id_devices"
        if op.get_bind().dialect.name == "sqlite"
        else "remote_sessions_device_id_fkey"
    )


def upgrade():
    op.add_column("remote_sessions", sa.Column("request_id", sa.String(36), nullable=True))
    op.add_column("remote_sessions", sa.Column("device_name", sa.String(255), nullable=True))
    with op.batch_alter_table(
        "remote_sessions",
        naming_convention={"fk": "fk_%(table_name)s_%(column_0_name)s_%(referred_table_name)s"},
    ) as batch:
        batch.drop_constraint(_device_fk(), type_="foreignkey")
        batch.alter_column("device_id", existing_type=sa.Uuid(), nullable=True)
        batch.create_foreign_key(
            "fk_remote_sessions_device_id_devices",
            "devices",
            ["device_id"],
            ["id"],
            ondelete="SET NULL",
        )
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
    orphan_count = (
        op.get_bind()
        .execute(sa.text("SELECT COUNT(*) FROM remote_sessions WHERE device_id IS NULL"))
        .scalar_one()
    )
    if orphan_count:
        raise RuntimeError("Cannot downgrade while historical sessions reference deleted devices")
    op.drop_index(
        "ix_remote_session_diagnostic_session", table_name="remote_session_diagnostic_events"
    )
    op.drop_table("remote_session_diagnostic_events")
    with op.batch_alter_table(
        "remote_sessions",
        naming_convention={"fk": "fk_%(table_name)s_%(column_0_name)s_%(referred_table_name)s"},
    ) as batch:
        batch.drop_constraint("fk_remote_sessions_device_id_devices", type_="foreignkey")
        batch.alter_column("device_id", existing_type=sa.Uuid(), nullable=False)
        batch.create_foreign_key(
            "fk_remote_sessions_device_id_devices",
            "devices",
            ["device_id"],
            ["id"],
            ondelete="CASCADE",
        )
    op.drop_column("remote_sessions", "device_name")
    op.drop_column("remote_sessions", "request_id")
