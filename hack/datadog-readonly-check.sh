#!/usr/bin/env bash
# Verify that the Datadog credentials in the environment are valid and that the
# application key is read-only.
#
#   DD_API_KEY   Datadog API key
#   DD_APP_KEY   Datadog application key
#   DD_SITE      Datadog site, e.g. datadoghq.com or us5.datadoghq.com
#
# Optional:
#   DD_API_URL      base URL of the API, for tests against a local fake only
#                   (test/fakedatadog). Accepted only as http://127.0.0.1:PORT,
#                   http://localhost:PORT or http://[::1]:PORT, so a real key can
#                   never be sent to another host through it.
#   READONLY_ALLOW  extended regex a scope name must match to count as read-only
#                   (default: '(_read|_view|_read_data)$'). Any scope that does not match
#                   fails the check.
#
# Flags:
#   --allow-unverified  exit 0 (with a warning) when the application key scopes
#                       cannot be read through the API, instead of exiting 3.
#
# How scopes are read: GET /api/v2/current_user/application_keys lists the
# application keys of the user that owns the key. The response never contains
# the key itself, only its last four characters, so the entry for this key is
# found by matching those. An entry with "scopes": null is an unscoped key that
# inherits every permission of its owner, so it is treated as NOT read-only.
#
# Exit codes: 0 read-only (or unverified and allowed), 1 invalid key or scope
# that allows writes, 2 usage or missing variable, 3 scopes cannot be verified.
#
# Keys are only passed to curl through a config file on a pipe, never as
# arguments, and are never printed. API output is treated as data.
set -euo pipefail

allow_unverified=0
for arg in "$@"; do
  case "$arg" in
    --allow-unverified) allow_unverified=1 ;;
    -h | --help)
      sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "error: unknown argument: $arg" >&2
      exit 2
      ;;
  esac
done

for v in DD_API_KEY DD_APP_KEY DD_SITE; do
  if [[ -z "${!v:-}" ]]; then
    echo "error: $v is not set (export DD_API_KEY, DD_APP_KEY and DD_SITE)" >&2
    exit 2
  fi
done
if ! command -v jq >/dev/null 2>&1; then
  echo "error: jq is required" >&2
  exit 2
fi
if [[ ! "$DD_SITE" =~ ^[A-Za-z0-9.-]+$ ]]; then
  echo "error: DD_SITE must be a hostname such as datadoghq.com" >&2
  exit 2
fi

# Keys go into a curl config file; refuse anything that could break the quoting.
for v in DD_API_KEY DD_APP_KEY; do
  if [[ ! "${!v}" =~ ^[A-Za-z0-9_-]+$ ]]; then
    echo "error: $v contains unexpected characters" >&2
    exit 2
  fi
done

readonly_allow="${READONLY_ALLOW:-(_read|_view|_read_data)$}"
base="https://api.${DD_SITE}"
if [[ -n "${DD_API_URL:-}" ]]; then
  if [[ ! "$DD_API_URL" =~ ^http://(127\.0\.0\.1|localhost|\[::1\]):[0-9]+$ ]]; then
    echo "error: DD_API_URL is only accepted for loopback addresses such as http://127.0.0.1:PORT (it exists for tests against a fake)" >&2
    exit 2
  fi
  base="$DD_API_URL"
fi

tmp_body="$(mktemp)"
trap 'rm -f "$tmp_body"' EXIT

# dd_get PATH [app]: GET base+PATH. Sets resp_status and resp_body.
# Headers go through a curl config on a process-substitution pipe (printf is a
# builtin) so the keys never appear in any process argument list.
resp_status=""
resp_body=""
dd_get() {
  local path="$1" with_app="${2:-}"
  resp_status=""
  resp_body=""
  if ! resp_status="$(curl --silent --show-error --max-time 20 \
    --output "$tmp_body" --write-out '%{http_code}' \
    --config <(
      printf 'header = "DD-API-KEY: %s"\n' "$DD_API_KEY"
      if [[ -n "$with_app" ]]; then
        printf 'header = "DD-APPLICATION-KEY: %s"\n' "$DD_APP_KEY"
      fi
      printf 'url = "%s%s"\n' "$base" "$path"
    ) 2>/dev/null)"; then
    resp_status="000"
    return 1
  fi
  resp_body="$(cat "$tmp_body")"
}

# 1. API key validity.
if ! dd_get /api/v1/validate; then
  echo "error: request to $base failed (network or TLS error)" >&2
  exit 1
fi
if [[ "$resp_status" == "200" ]] && [[ "$(jq -r '.valid // false' <<<"$resp_body" 2>/dev/null)" == "true" ]]; then
  echo "API key: valid for site $DD_SITE"
else
  echo "API key: INVALID (HTTP $resp_status) for site $DD_SITE" >&2
  exit 1
fi

# 2. Application key scopes.
unverified() {
  echo "cannot verify application key scopes via API; verify in the Datadog UI" >&2
  echo "reason: $1" >&2
  if [[ "$allow_unverified" == "1" ]]; then
    echo "warning: continuing because --allow-unverified was given" >&2
    exit 0
  fi
  exit 3
}

last4="${DD_APP_KEY: -4}"
keys='[]'
page=0
while ((page < 10)); do
  if ! dd_get "/api/v2/current_user/application_keys?page%5Bsize%5D=100&page%5Bnumber%5D=${page}" app; then
    unverified "request failed"
  fi
  if [[ "$resp_status" != "200" ]]; then
    unverified "application key listing returned HTTP $resp_status"
  fi
  if ! jq -e '.data | type == "array"' <<<"$resp_body" >/dev/null 2>&1; then
    unverified "unexpected response shape"
  fi
  keys="$(jq -c --argjson acc "$keys" '$acc + .data' <<<"$resp_body")"
  if [[ "$(jq '.data | length' <<<"$resp_body")" -lt 100 ]]; then
    break
  fi
  page=$((page + 1))
done

# Entries whose last4 equals ours. Several can match; every match must pass.
matches="$(jq -c --arg l4 "$last4" '[.[] | select(.attributes.last4 == $l4)]' <<<"$keys")"
count="$(jq 'length' <<<"$matches")"
if [[ "$count" == "0" ]]; then
  unverified "this key is not in the current user's application key list"
fi

fail=0
while IFS= read -r entry; do
  name="$(jq -r '.attributes.name // "unnamed"' <<<"$entry" | tr -cd '[:print:]')"
  if [[ "$(jq '.attributes.scopes == null' <<<"$entry")" == "true" ]]; then
    echo "application key '$name': UNSCOPED (inherits all permissions of its owner)" >&2
    fail=1
    continue
  fi
  while IFS= read -r scope; do
    scope="$(tr -cd '[:print:]' <<<"$scope")"
    if [[ "$scope" =~ $readonly_allow ]]; then
      echo "application key '$name': scope $scope (read-only)"
    else
      echo "application key '$name': scope $scope ALLOWS WRITES" >&2
      fail=1
    fi
  done < <(jq -r '.attributes.scopes[]' <<<"$entry")
done < <(jq -c '.[]' <<<"$matches")

if [[ "$count" != "1" ]]; then
  echo "warning: $count application keys share the last four characters; all were checked" >&2
fi
if [[ "$fail" != "0" ]]; then
  echo "FAIL: application key is not read-only" >&2
  exit 1
fi
echo "OK: application key scopes are read-only"
