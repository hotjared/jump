"""Optional local identities and one-time setup."""

import sqlalchemy as sa

from alembic import op

revision = "0010"
down_revision = "0009"
branch_labels = None
depends_on = None


def upgrade():
    with op.batch_alter_table("users") as batch:
        batch.alter_column("oidc_issuer", existing_type=sa.String(512), nullable=True)
        batch.alter_column("oidc_subject", existing_type=sa.String(512), nullable=True)
        batch.add_column(sa.Column("local_username", sa.String(100), nullable=True))
        batch.add_column(sa.Column("password_hash", sa.String(255), nullable=True))
        batch.create_unique_constraint("uq_users_local_username", ["local_username"])
    op.create_table(
        "local_setup",
        sa.Column("id", sa.Integer(), primary_key=True),
        sa.Column("token_hash", sa.String(64), nullable=False),
    )
    op.create_table(
        "local_login_failures",
        sa.Column("username", sa.String(100), primary_key=True),
        sa.Column("attempts", sa.Integer(), nullable=False),
        sa.Column("locked_until", sa.DateTime(timezone=True)),
    )


def downgrade():
    op.drop_table("local_login_failures")
    op.drop_table("local_setup")
    with op.batch_alter_table("users") as batch:
        batch.drop_constraint("uq_users_local_username", type_="unique")
        batch.drop_column("password_hash")
        batch.drop_column("local_username")
        batch.alter_column("oidc_subject", existing_type=sa.String(512), nullable=False)
        batch.alter_column("oidc_issuer", existing_type=sa.String(512), nullable=False)
