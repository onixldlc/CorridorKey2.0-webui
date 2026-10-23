#!/bin/sh
# Puts the inference stack in the volume, once.
#
# torch and its CUDA libraries are 3.3 GB. Baked into the image they are 3.3 GB
# of registry pull on every version of this UI, however small the change, and
# a single enormous layer that pulls in one stream. In the volume they are
# downloaded once per box, at the full speed of its link, and every later
# container start — including after an image upgrade — finds them already
# there and skips straight to the server.
#
# This is the same bargain the weights already make, for the same reason.
set -eu

VENV="${CKWEB_VENV:-/data/venv}"
WHEELS="${CKWEB_WHEELS:-/data/wheels}"
STAMP="$VENV/.ck2-stack"
PY="$VENV/bin/python"

# What the stamp records. A change to any of it means the venv on this volume
# is not the one this image wants, so it is rebuilt.
SPEC="torch=${TORCH_VERSION:-} torchvision=${TORCHVISION_VERSION:-} index=${TORCH_INDEX_URL:-} py=$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')"

# One bootstrap at a time. A restart while the first is still downloading
# would otherwise install into a half-built venv.
LOCK="${VENV%/*}/.ck2-bootstrap.lock"
if ! mkdir "$LOCK" 2>/dev/null; then
	printf 'ck-web: another bootstrap is already running (%s exists)\n' "$LOCK" >&2
	exit 0
fi
trap 'rmdir "$LOCK" 2>/dev/null || true' EXIT INT TERM

if [ -x "$PY" ] && [ -f "$STAMP" ] && [ "$(cat "$STAMP")" = "$SPEC" ]; then
	printf 'ck-web: inference stack already in %s, nothing to download\n' "$VENV"
	# Worth the few seconds: "cuda False" here is the difference between a
	# misconfigured --gpus flag and a mystery at the first job.
	"$PY" -c 'import torch; print("ck-web: torch", torch.__version__, "- cuda", torch.cuda.is_available())' \
		|| printf 'ck-web: torch did not import — delete %s and restart to rebuild it\n' "$VENV" >&2
	exit 0
fi

if [ -x "$PY" ]; then
	printf 'ck-web: %s was built for different versions, rebuilding it\n' "$VENV"
	printf '        have: %s\n' "$(cat "$STAMP" 2>/dev/null || echo 'no stamp')"
	printf '        want: %s\n' "$SPEC"
	rm -rf "$VENV"
fi

printf 'ck-web: installing the inference stack into %s — first start only\n' "$VENV"
printf '        the UI is up already; the keyer reports not ready until this finishes\n'

mkdir -p "$WHEELS"

# Resolve the heavy wheels to direct URLs so aria2c can fetch them in
# parallel. pip would take them one stream at a time.
python3 /usr/local/bin/wheels.py "$WHEELS/aria2.txt"

# --continue and --auto-file-renaming=false make a second run resume the cache
# rather than refetch it, so an interrupted first start is not paid for twice.
# 16 is aria2's own ceiling per server, not a number picked here.
aria2c \
	--input-file="$WHEELS/aria2.txt" \
	--dir="$WHEELS" \
	--max-connection-per-server=16 \
	--split=16 \
	--min-split-size=8M \
	--max-concurrent-downloads=4 \
	--continue=true \
	--auto-file-renaming=false \
	--allow-overwrite=false \
	--summary-interval=10 \
	--console-log-level=warn \
	--check-integrity=true

python3 -m venv "$VENV"

"$PY" -m pip install --no-cache-dir --upgrade pip

# have <module>... — true when this venv can already import all of them.
have() {
	"$PY" -c "import $(printf '%s' "$*" | tr ' ' ',')" 2>/dev/null
}

# The stamp is only written once everything below has succeeded, so a restart
# part-way through comes back here with some of it already installed. Each step
# asks first: re-running pip would be minutes of work and, for the git
# dependency in requirements.txt, another clone of a repository already on disk.

# --no-deps: the dependency set was already resolved from torch's own metadata
# and fetched above, minus the four nothing here uses. Letting pip resolve
# again would pull those back.
if have torch torchvision; then
	printf 'ck-web: torch and torchvision are already installed, skipping\n'
else
	"$PY" -m pip install --no-cache-dir --no-deps --no-index \
		--find-links "$WHEELS" \
		"$WHEELS"/*.whl
fi

# torch's own pure-Python dependencies, as its metadata declares them. Small,
# and they need real resolution, so pip fetches these itself.
#
# pip ends this step with "ERROR: pip's dependency resolver does not currently
# take into account all the packages that are installed", naming nccl,
# nvshmem, cusparselt and triton as missing. That is expected and nothing is
# broken: wheels.py drops those four deliberately, because a single-GPU keyer
# that never calls torch.compile does not touch them, and they are 2 GB.
if have filelock jinja2 sympy fsspec networkx; then
	printf "ck-web: torch's pure-Python dependencies are already installed, skipping\n"
else
	printf 'ck-web: the "dependency resolver" ERROR pip prints next is expected —\n'
	printf '        nccl, nvshmem, cusparselt and triton are dropped on purpose\n'
	"$PY" -m pip install --no-cache-dir -r "$WHEELS/aria2.txt.deps"
fi

if have cv2 transformers timm safetensors utils3d; then
	printf 'ck-web: the keyer requirements are already installed, skipping\n'
else
	"$PY" -m pip install --no-cache-dir -r "${CKWEB_ROOT:-/opt/corridorkey}/requirements.txt"
fi

# Fail loudly here rather than at the first job, and prove CUDA is reachable
# while the log is still being read.
"$PY" -c 'import torch, cv2, transformers, timm, safetensors, utils3d; \
print("ck-web: torch", torch.__version__, "- cuda", torch.cuda.is_available())'

printf '%s' "$SPEC" > "$STAMP"

# The wheels are kept: they make the next rebuild, on a torch bump or a fresh
# venv, close to instant. Set CKWEB_KEEP_WHEELS=0 to reclaim the 3.3 GB.
if [ "${CKWEB_KEEP_WHEELS:-1}" = "0" ]; then
	rm -rf "$WHEELS"
	printf 'ck-web: wheel cache removed\n'
else
	printf 'ck-web: wheel cache kept in %s (%s)\n' \
		"$WHEELS" "$(du -sh "$WHEELS" 2>/dev/null | cut -f1)"
fi

printf 'ck-web: inference stack ready\n'
