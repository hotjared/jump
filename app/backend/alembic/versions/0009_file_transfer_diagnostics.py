"""Retain historical file transfers and persist fixed lifecycle stages."""

import sqlalchemy as sa

from alembic import op

revision = "0009"
down_revision = "0008"
branch_labels = None
depends_on = None


def _device_fk():
    return (
        "fk_file_transfers_device_id_devices"
        if op.get_bind().dialect.name == "sqlite"
        else "file_transfers_device_id_fkey"
    )


def upgrade():
    op.add_column("file_transfers", sa.Column("device_name", sa.String(255), nullable=True))
    with op.batch_alter_table(
        "file_transfers",
        naming_convention={"fk": "fk_%(table_name)s_%(column_0_name)s_%(referred_table_name)s"},
    ) as batch:
        batch.drop_constraint(_device_fk(), type_="foreignkey")
        batch.alter_column("device_id", existing_type=sa.Uuid(), nullable=True)
        batch.create_foreign_key(
            "fk_file_transfers_device_id_devices",
            "devices",
            ["device_id"],
            ["id"],
            ondelete="SET NULL",
        )
    op.create_table(
        "file_transfer_diagnostic_events",
        sa.Column("id", sa.Uuid(), primary_key=True),
        sa.Column(
            "transfer_id",
            sa.Uuid(),
            sa.ForeignKey("file_transfers.id", ondelete="CASCADE"),
            nullable=False,
        ),
        sa.Column("stage", sa.String(40), nullable=False),
        sa.Column("created_at", sa.DateTime(timezone=True), nullable=False),
        sa.UniqueConstraint("transfer_id", "stage", name="uq_file_transfer_diagnostic_stage"),
    )
    op.create_index(
        "ix_file_transfer_diagnostic_transfer", "file_transfer_diagnostic_events", ["transfer_id"]
    )


def downgrade():
    orphan_count = (
        op.get_bind()
        .execute(sa.text("SELECT COUNT(*) FROM file_transfers WHERE device_id IS NULL"))
        .scalar_one()
    )
    if orphan_count:
        raise RuntimeError("Cannot downgrade while historical transfers reference deleted devices")
    op.drop_index(
        "ix_file_transfer_diagnostic_transfer", table_name="file_transfer_diagnostic_events"
    )
    op.drop_table("file_transfer_diagnostic_events")
    with op.batch_alter_table(
        "file_transfers",
        naming_convention={"fk": "fk_%(table_name)s_%(column_0_name)s_%(referred_table_name)s"},
    ) as batch:
        batch.drop_constraint("fk_file_transfers_device_id_devices", type_="foreignkey")
        batch.alter_column("device_id", existing_type=sa.Uuid(), nullable=False)
        batch.create_foreign_key(
            "fk_file_transfers_device_id_devices",
            "devices",
            ["device_id"],
            ["id"],
            ondelete="CASCADE",
        )
    op.drop_column("file_transfers", "device_name")
