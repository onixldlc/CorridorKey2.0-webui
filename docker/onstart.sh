#!/bin/bash
# Starts ck-web when the image ENTRYPOINT will not run.
#
# Rented-box consoles (Vast.ai and friends) offer a launch mode that replaces
# the image entrypoint with their own shell server, so picking "SSH" means the
# server never starts. Point that console's on-start script at this file and
# both happen: their sshd is PID 1, ck-web runs beside it.
#
# Everything this writes goes to three places at once: this box's log file,
# the console's own instance log, and the terminal of whoever is watching.
# A server whose output only exists in a file nobody mentioned is a server
# that looks hung, which is the one failure mode a rented box cannot afford.
set -u

LOG="${CKWEB_LOG:-/var/log/ck-web.log}"
touch "$LOG"

# The console's environment table lands in /etc/environment, not in this
# shell, so the ENV defaults baked into the image are not visible here.
set -a
[ -f /etc/environment ] && . /etc/environment
set +a

: "${CKWEB_ADDR:=:8080}"
: "${CKWEB_ROOT:=/opt/corridorkey}"
: "${CKWEB_CLIPS:=/data/ClipsForInference}"
: "${CKWEB_PYTHON:=/opt/venv/bin/python}"
: "${CKWEB_MODE:=auto}"
: "${CKWEB_TOKEN:=auto}"
export CKWEB_ADDR CKWEB_ROOT CKWEB_CLIPS CKWEB_PYTHON CKWEB_MODE CKWEB_TOKEN
export OPENCV_IO_ENABLE_OPENEXR=1

# Run it, or show it. Being told "already running" and nothing else is the
# least useful thing this script could say: the reason anyone runs it twice is
# that they want to know what the server is doing, so the second run tails it.
# Following only when there is a terminal keeps the console's boot-time run
# from blocking forever on a log that never ends.
if pgrep -x ck-web >/dev/null 2>&1; then
    echo "onstart: ck-web is already running (pid $(pgrep -x ck-web | tr '\n' ' '))"
    echo "onstart: $(/usr/local/bin/ck-url)"
    if [ -t 1 ]; then
        echo "onstart: following $LOG — Ctrl-C stops watching, not the server"
        exec /usr/local/bin/ck-logs
    fi
    /usr/local/bin/ck-logs -n 200
    exit $?
fi

# PID 1's stdout is what the rented box shows as the instance log. In SSH
# launch mode PID 1 is the console's own shell server, so the server's output
# has to be handed to it explicitly or the instance log stays empty while the
# server talks to a file nobody is tailing.
CONSOLE=/dev/null
if [ -w /proc/1/fd/1 ]; then
    CONSOLE=/proc/1/fd/1
fi

nohup setsid /usr/local/bin/entrypoint.sh < /dev/null 2>&1 \
    | tee -a "$LOG" > "$CONSOLE" &

# Wait for the banner, so a login right after boot does not race the server.
for _ in $(seq 1 90); do
    grep -q 'http://' "$LOG" && break
    sleep 1
done

if ! grep -q 'http://' "$LOG"; then
    echo "onstart: ck-web printed no banner in 90s. Last of $LOG:" | tee -a "$CONSOLE"
    tail -20 "$LOG" | tee -a "$CONSOLE"
    exit 1
fi

# The URL is NOT baked in here. -token auto keeps its secret in the volume
# now, so ck-url reads the live one every time it is asked; a copy frozen at
# first boot was exactly what went stale and got rejected by the UI.
mkdir -p /etc/profile.d
cat > /etc/profile.d/ck-web.sh <<'PROFILE'
echo
echo "ck-web   $(ck-url)"
echo "         ck-logs    follow the server log"
echo "         ck-url     print this link again"
echo "         tunnel:    ssh -L 8080:localhost:8080 <this box>"
echo
PROFILE
chmod +x /etc/profile.d/ck-web.sh

echo "onstart: ck-web up — $(ck-url)" | tee -a "$LOG" > "$CONSOLE"
