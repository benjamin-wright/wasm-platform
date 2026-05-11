# Parameterised migrations image — one Dockerfile per repo, one build per app.
#
# Usage:
#   docker build --build-arg APP=sql-hello \
#     -f examples/sql-hello/migrations.Dockerfile \
#     -t registry.example.com/sql-hello-migrations:v1 .
#
# The build context must be the repository root so that ${APP}/migrations/ resolves
# correctly. Bump DB_MIGRATIONS_VERSION to match the deployed db-operator version.

ARG DB_MIGRATIONS_VERSION=v1.0.10
FROM docker.io/benwright/db-migrations:${DB_MIGRATIONS_VERSION}

ARG APP
COPY ${APP}/migrations/ /migrations/
