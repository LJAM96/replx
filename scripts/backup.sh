#!/bin/sh
# Replx Edge backup: Postgres dump + secret material reminder.
# Usage: POSTGRES_PASSWORD=... ./scripts/backup.sh [outdir]
# Valkey and artwork cache are excluded by design (rebuilt from origin).
set -eu
OUT=${1:-./backups/$(date +%Y%m%dT%H%M%SZ)}
mkdir -p "$OUT"
echo "dumping postgres to $OUT/replx_edge.sql"
PGPASSWORD="${POSTGRES_PASSWORD:?POSTGRES_PASSWORD required}" pg_dump -h "${POSTGRES_HOST:-localhost}" -U "${POSTGRES_USER:-replx_edge}" -d "${POSTGRES_DB:-replx_edge}" -F c -f "$OUT/replx_edge.dump"
cat > "$OUT/README.txt" <<'EOF'
Replx Edge backup contents:
- replx_edge.dump (Postgres custom format; restore with pg_restore)
Required separately (never stored here):
- REPLX_EDGE_SECRET_KEY (loss makes encrypted owner credentials unrecoverable)
- .env or equivalent secret source
- Replx Edge owner key material notes
Excluded by design: Valkey data, artwork cache, diagnostics files.
EOF
echo "backup complete: $OUT (remember REPLX_EDGE_SECRET_KEY separately)"
