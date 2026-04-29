#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${PROXYHARBOR_BASE_URL:-http://localhost:18080}"
ADMIN_KEY="${PROXYHARBOR_ADMIN_KEY:-dev-admin-key-min-32-chars-long!!!!}"
KEY_PEPPER="${PROXYHARBOR_KEY_PEPPER:-dev-key-pepper-min-32-bytes-random!!!!}"
REQUESTS="${REQUESTS:-500}"
CONCURRENCY="${CONCURRENCY:-32}"
PROXIES="${PROXIES:-16}"
OPERATION="${OPERATION:-mixed}"
OUTPUT="${OUTPUT:-json}"
OUT="${OUT:-}"
SKIP_DOCKER="${SKIP_DOCKER:-false}"

export PROXYHARBOR_BASE_URL="$BASE_URL"
export PROXYHARBOR_ADMIN_KEY="$ADMIN_KEY"
export PROXYHARBOR_KEY_PEPPER="$KEY_PEPPER"
export PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true
export PROXYHARBOR_AUTH_REFRESH_INTERVAL=1s
export PROXYHARBOR_HOST_PORT="${PROXYHARBOR_HOST_PORT:-18080}"

args=(run ./tools/singlebench -base-url "$BASE_URL" -admin-key "$ADMIN_KEY" -requests "$REQUESTS" -concurrency "$CONCURRENCY" -proxies "$PROXIES" -operation "$OPERATION" -output "$OUTPUT")
if [[ "$SKIP_DOCKER" != "true" ]]; then
  args+=(-docker)
fi
if [[ -n "$OUT" ]]; then
  args+=(-out "$OUT")
fi

go "${args[@]}"
