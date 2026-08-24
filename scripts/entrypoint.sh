#!/bin/sh
# Entrypoint script for POS-API service

set -e

# Use direct PostgreSQL URL for migrate/seed to bypass PgBouncer transaction mode.
MIGRATE_URL="${POSTGRES_MIGRATE_URL:-$POSTGRES_URL}"

echo "=========================================="
echo "POS-API Service Startup"
echo "=========================================="

echo "Waiting for database and running migrations..."
MAX_RETRIES=60
RETRY_COUNT=0

# Captured (not swallowed) so a real migration failure is visible on every attempt -- the
# liveness probe usually kills this container long before MAX_RETRIES is ever reached.
until MIGRATE_OUTPUT=$(POSTGRES_URL="$MIGRATE_URL" /usr/local/bin/pos-migrate 2>&1) || [ $RETRY_COUNT -eq $MAX_RETRIES ]; do
  RETRY_COUNT=$((RETRY_COUNT+1))
  echo "Migration attempt $RETRY_COUNT/$MAX_RETRIES failed:"
  echo "$MIGRATE_OUTPUT"
  sleep 5
done

if [ $RETRY_COUNT -eq $MAX_RETRIES ]; then
  echo "Migration failed after $MAX_RETRIES attempts. Last error:"
  echo "$MIGRATE_OUTPUT"
  exit 1
fi

echo "Migrations applied successfully"

echo ""
echo "=========================================="
echo "Running seed (idempotent)"
echo "=========================================="
POSTGRES_URL="$MIGRATE_URL" /usr/local/bin/pos-seed || echo "Seed completed with warnings (non-fatal)"

echo ""
echo "=========================================="
echo "Starting POS-API server"
echo "=========================================="
echo ""

exec /usr/local/bin/pos-api
