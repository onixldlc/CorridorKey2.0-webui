#!/bin/sh
# Prepares the data volume and starts the server.
#
# The keyer is baked into the image at /opt/corridorkey, cloned at build time
# from this fork's `upstream` branch. The weights, the clips and the inference
# stack live in the volume, so replacing the container costs nothing and an
# image upgrade does not re-download 3.3 GB of torch.
#
# A container start is still offline-safe once the volume is populated: the
# bootstrap skips on a stamp match and the modules are only fetched when the
# Modules page asks.
set -eu

ROOT="${CKWEB_ROOT:-/opt/corridorkey}"
CLIPS="${CKWEB_CLIPS:-/data/ClipsForInference}"
MODE="${CKWEB_MODE:-auto}"
VENV="${CKWEB_VENV:-/data/venv}"
PY="${CKWEB_PYTHON:-$VENV/bin/python}"

# /opt/corridorkey/weights and /vfms are symlinks into /data; a freshly mounted
# host directory has none of this, so make the targets before anything reads
# through them.
mkdir -p "$CLIPS" /data/weights /data/vfms/cradio /data/vfms/moge /data/vfms/rvm

if [ -f "$ROOT/.ck2-source-revision" ]; then
	printf 'ck-web: CorridorKey 2.0 runtime baked into this image from\n'
	sed 's/^/        /' "$ROOT/.ck2-source-revision"
fi

if [ -z "${CKWEB_TOKEN:-}" ] && [ "$MODE" != "dev" ]; then
	printf 'ck-web: CKWEB_TOKEN is unset — every API route is open to whatever can reach %s\n' \
		"${CKWEB_ADDR:-:8080}" >&2
	printf '        Run with -e CKWEB_TOKEN=auto to have one generated; it is printed below.\n' >&2
fi

if [ "$MODE" = "dev" ]; then
	# A dev run drives the mock keyer, which needs no torch at all.
	printf 'ck-web: dev run — mock keyer, downloads disabled\n'
elif [ ! -f "$ROOT/infer.py" ]; then
	printf 'ck-web: no infer.py at %s — the mock provider will be used.\n' "$ROOT"
	printf '        Something has overmounted the runtime; CKWEB_ROOT must point at a checkout.\n'
else
	# Backgrounded so the page and the log are reachable immediately. On a
	# volume that already has the stack this returns in about a second; on a
	# fresh one it downloads 3.3 GB and the keyer reports itself not ready
	# until it finishes.
	/usr/local/bin/bootstrap.sh 2>&1 &
fi

exec /usr/local/bin/ck-web "$@"
