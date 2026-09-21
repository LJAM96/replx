#!/bin/sh
# Replx Edge restore into a clean stack: verifies server identity, owner auth
# metadata, users, policies and compatibility rows after pg_restore.
# Usage: POSTGRES_PASSWORD=... ./scripts/restore.sh <dumpfile>
set -eu
DUMP=${1:?dump file required}
echo "restoring $DUMP"
PGPASSWORD="${POSTGRES_PASSWORD:?POSTGRES_PASSWORD required}" pg_restore --clean --if-exists -h "${POSTGRES_HOST:-localhost}" -U "${POSTGRES_USER:-replx_edge}" -d "${POSTGRES_DB:-replx_edge}" "$DUMP"
PSQL="psql -h ${POSTGRES_HOST:-localhost} -U ${POSTGRES_USER:-replx_edge} -d ${POSTGRES_DB:-replx_edge} -v ON_ERROR_STOP=1 -tA"
echo "-- server identity"
PGPASSWORD="$POSTGRES_PASSWORD" $PSQL -c "SELECT count(*), count(*) FILTER (WHERE enabled) FROM plex_servers;"
echo "-- owner auth metadata (ciphertext present, never plaintext)"
PGPASSWORD="$POSTGRES_PASSWORD" $PSQL -c "SELECT count(*), count(pms_access_token_ciphertext) FROM plex_owner_credentials;"
echo "-- users / policies / compat"
PGPASSWORD="$POSTGRES_PASSWORD" $PSQL -c "SELECT (SELECT count(*) FROM plex_identities), (SELECT count(*) FROM policies), (SELECT count(*) FROM compatibility_profiles);"
echo "restore verification complete"
