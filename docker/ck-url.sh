#!/bin/sh
# Prints the link to open, with the current token in it.
#
# The token is read from the volume, not from a copy made at boot: -token auto
# saves it beside the clips and reuses it, so this answer stays true across a
# restart, a container replacement and an image upgrade.
set -u

CLIPS="${CKWEB_CLIPS:-/data/ClipsForInference}"
STORE="$(dirname "$CLIPS")/.ck-web-token"

TOKEN="${CKWEB_TOKEN:-auto}"
if [ "$TOKEN" = "auto" ] || [ -z "$TOKEN" ]; then
    TOKEN=""
    [ -r "$STORE" ] && TOKEN="$(tr -d '[:space:]' < "$STORE")"
fi

PORT="${CKWEB_ADDR##*:}"
[ -n "$PORT" ] || PORT=8080

# A rented box publishes the container's 8080 on some other port, so prefer
# the public pair the console puts in the environment.
HOST="localhost"
if [ -n "${PUBLIC_IPADDR:-}" ] && [ -n "${VAST_TCP_PORT_8080:-}" ]; then
    HOST="$PUBLIC_IPADDR"
    PORT="$VAST_TCP_PORT_8080"
fi

if [ -z "$TOKEN" ]; then
    printf 'http://%s:%s/\n' "$HOST" "$PORT"
    exit 0
fi
printf 'http://%s:%s/?t=%s\n' "$HOST" "$PORT" "$TOKEN"
