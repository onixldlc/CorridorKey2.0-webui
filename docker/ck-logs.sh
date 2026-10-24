#!/bin/sh
# Follows the server log. With -n it prints that many lines and stops.
#
# This exists so the answer to "what is it doing" is a command, not a path
# somebody has to remember. The same lines are already on the console's
# instance log; this is for when you are on the box over SSH.
set -u

LOG="${CKWEB_LOG:-/var/log/ck-web.log}"

if [ ! -f "$LOG" ]; then
    printf 'ck-logs: nothing at %s yet.\n' "$LOG" >&2
    if pgrep -x ck-web >/dev/null 2>&1; then
        printf '         ck-web is running, so it was started without onstart.sh —\n' >&2
        printf '         its output went wherever that start sent it (try: docker logs <container>).\n' >&2
    else
        printf '         ck-web is NOT running. Start it with: onstart.sh\n' >&2
    fi
    exit 1
fi

if [ "${1:-}" = "-n" ]; then
    tail -n "${2:-200}" "$LOG"
    exit 0
fi
exec tail -n "${1:-200}" -f "$LOG"
