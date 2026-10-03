"""Keep interactive session identity separate from the agent service identity."""

import sqlalchemy as sa

from alembic import op

revision = "0013"
down_revision = "0012"
branch_labels = None
depends_on = None


def upgrade():
    op.add_column("devices", sa.Column("interactive_user", sa.String(255), nullable=True))


def downgrade():
    op.drop_column("devices", "interactive_user")
