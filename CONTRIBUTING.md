# Contributing

Open an issue before large architectural changes. Keep the scope centered on remote access rather than RMM features. Use feature branches and pull requests. Do not commit real secrets, agent identity files, database dumps or enrollment tokens.

Run the backend, frontend, Go and Compose checks in [deployment](docs/deployment.md). Add tests for authorization and failure paths around security changes. Schema changes require an Alembic revision. Describe trust-boundary changes in the PR.
