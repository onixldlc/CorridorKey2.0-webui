#!/bin/sh
# Prepares the data volume and starts the server.
#
# The keyer is baked into the image at /opt/corridorkey, cloned at build time
# from this fork's `upstream` branch. Only the weights and the clips live in
# the volume, so replacing the container costs nothing and a container start
# is offline-safe: modules are fetched on request from the Modules page, never
# from here.
set -eu

ROOT="${CKWEB_ROOT:-/opt/corridorkey}"
CLIPS="${CKWEB_CLIPS:-/data/ClipsForInference}"
MODE="${CKWEB_MODE:-auto}"
PY="${CKWEB_PYTHON:-/opt/venv/bin/python}"

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
	printf 'ck-web: dev run — mock keyer, downloads disabled\n'
elif [ ! -f "$ROOT/infer.py" ]; then
	printf 'ck-web: no infer.py at %s — the mock provider will be used.\n' "$ROOT"
	printf '        Something has overmounted the runtime; CKWEB_ROOT must point at a checkout.\n'
elif [ -x "$PY" ]; then
	# Worth the few seconds it costs: "cuda False" printed here is the
	# difference between a misconfigured --gpus flag and a mystery at the
	# first job.
	"$PY" -c 'import torch; print("ck-web: torch", torch.__version__, "- cuda", torch.cuda.is_available())' \
		|| printf 'ck-web: torch did not import — the image is broken, please report it\n' >&2
fi

exec /usr/local/bin/ck-web "$@"
