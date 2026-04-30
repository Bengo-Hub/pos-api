#!/bin/sh
# Entrypoint script for POS-API service

set -e

echo "=========================================="
echo "POS-API Service Startup"
echo "=========================================="

echo "Waiting for database and running migrations..."
MAX_RETRIES=60
RETRY_COUNT=0

until /usr/local/bin/pos-migrate > /dev/null 2>&1 || [ $RETRY_COUNT -eq $MAX_RETRIES ]; do
  RETRY_COUNT=$((RETRY_COUNT+1))
  echo "Database not ready yet... (attempt $RETRY_COUNT/$MAX_RETRIES)"
  sleep 5
done

if [ $RETRY_COUNT -eq $MAX_RETRIES ]; then
  echo "Database connection timeout after $MAX_RETRIES attempts"
  exit 1
fi

echo "Migrations applied successfully"

echo ""
echo "=========================================="
echo "Running seed (idempotent)"
echo "=========================================="
/usr/local/bin/pos-seed || echo "Seed completed with warnings (non-fatal)"

echo ""
echo "=========================================="
echo "Starting POS-API server"
echo "=========================================="
echo ""

exec /usr/local/bin/pos-api
