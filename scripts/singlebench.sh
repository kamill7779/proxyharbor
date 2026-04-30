#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${PROXYHARBOR_BASE_URL:-http://localhost:18080}"
REQUESTS="${REQUESTS:-500}"
CONCURRENCY="${CONCURRENCY:-32}"
PROXIES="${PROXIES:-16}"
OPERATION="${OPERATION:-mixed}"
OUTPUT="${OUTPUT:-json}"
OUT="${OUT:-}"
SKIP_DOCKER="${SKIP_DOCKER:-false}"
ALLOW_INTERNAL="${ALLOW_INTERNAL:-false}"

export PROXYHARBOR_BASE_URL="$BASE_URL"
export PROXYHARBOR_AUTH_REFRESH_INTERVAL=1s
export PROXYHARBOR_HOST_PORT="${PROXYHARBOR_HOST_PORT:-18080}"

args=(run ./tools/singlebench -base-url "$BASE_URL" -requests "$REQUESTS" -concurrency "$CONCURRENCY" -proxies "$PROXIES" -operation "$OPERATION" -output "$OUTPUT")
if [[ -n "${PROXYHARBOR_ADMIN_KEY:-}" ]]; then
  args+=(-admin-key "$PROXYHARBOR_ADMIN_KEY")
fi
if [[ "$SKIP_DOCKER" != "true" ]]; then
  args+=(-docker)
fi
if [[ "$ALLOW_INTERNAL" == "true" ]]; then
  args+=(-allow-internal-proxy-endpoint)
fi
if [[ -n "$OUT" ]]; then
  args+=(-out "$OUT")
fi

go "${args[@]}"
