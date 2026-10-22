#!/usr/bin/env python3
"""Resolve the heavy half of the inference stack to direct wheel URLs.

pip fetches one file at a time over one connection. On a rented box that
leaves most of the link idle, which is the same problem the Modules page
already solved with aria2c — so the big wheels are resolved to URLs here,
handed to aria2c, and installed from the local directory afterwards. The
small pure-Python packages are left to pip, where resolution matters more
than bandwidth.

The CUDA pins are deliberately not written down anywhere: they are read from
torch's own Requires-Dist, so bumping TORCH_VERSION is the only edit a torch
upgrade needs. Four of them are dropped, because nothing in the keyer reaches
them: nccl and nvshmem are multi-GPU collectives, this runs on one card;
cusparselt is unreferenced; triton only matters to torch.compile, which the
runtime never calls.

Writes an aria2c input file on stdout, or to the path given as argv[1].
"""

import json
import os
import platform
import re
import sys
import urllib.parse
import urllib.request

TORCH_VERSION = os.environ.get("TORCH_VERSION", "2.9.1")
TORCHVISION_VERSION = os.environ.get("TORCHVISION_VERSION", "0.24.1")
INDEX_URL = os.environ.get("TORCH_INDEX_URL",
                           "https://download.pytorch.org/whl/cu128").rstrip("/")

# Dropped from torch's dependency set. See the module docstring.
DROP = {"nvidia-nccl-cu12", "nvidia-nvshmem-cu12",
        "nvidia-cusparselt-cu12", "triton"}

PY_TAG = f"cp{sys.version_info.major}{sys.version_info.minor}"
ARCH = platform.machine()


def fetch(url: str) -> bytes:
    with urllib.request.urlopen(url, timeout=60) as r:
        return r.read()


def from_torch_index(package: str, version: str):
    """Find (url, sha256) for a package on the PyTorch index."""
    page = fetch(f"{INDEX_URL}/{package}/").decode("utf-8", "replace")
    want = re.compile(
        rf'href="(?P<url>[^"]*/{re.escape(package)}-{re.escape(version)}'
        rf'(?:%2B[^-]+)?-{PY_TAG}-{PY_TAG}-manylinux[^"]*{ARCH}\.whl)'
        rf'#sha256=(?P<sha>[0-9a-f]{{64}})"')
    for m in want.finditer(page):
        return m.group("url"), m.group("sha")
    raise SystemExit(
        f"wheels.py: no {package} {version} wheel for {PY_TAG}/{ARCH} on {INDEX_URL}")


def from_pypi(package: str, version: str):
    """Find (url, sha256) for an exact version on PyPI."""
    data = json.loads(fetch(f"https://pypi.org/pypi/{package}/{version}/json"))
    for f in data["urls"]:
        name = f["filename"]
        if not name.endswith(".whl"):
            continue
        if ARCH in name or name.endswith("-any.whl"):
            return f["url"], f["digests"]["sha256"]
    raise SystemExit(f"wheels.py: no {package}=={version} wheel for {ARCH} on PyPI")


def torch_requirements(wheel_url: str):
    """Read torch's pinned CUDA dependencies from the wheel's own metadata.

    PEP 658 serves it next to the wheel, so this costs one small request
    rather than a download of the wheel itself.
    """
    body = fetch(wheel_url + ".metadata").decode("utf-8", "replace")
    pins, plain = {}, []
    for line in body.splitlines():
        if not line.startswith("Requires-Dist:"):
            continue
        raw = line.split(":", 1)[1].strip()
        if "extra ==" in raw:
            continue  # optional extras the runtime never asks for
        spec = raw.split(";", 1)[0].strip()
        m = re.fullmatch(r"([A-Za-z0-9_.\-]+)==([A-Za-z0-9_.\-+]+)", spec)
        if m and (m.group(1).startswith("nvidia-") or m.group(1) == "triton"):
            pins[m.group(1)] = m.group(2)
        else:
            plain.append(spec)
    return pins, plain


def main() -> int:
    entries = []

    torch_url, torch_sha = from_torch_index("torch", TORCH_VERSION)
    entries.append((torch_url, torch_sha))
    entries.append(from_torch_index("torchvision", TORCHVISION_VERSION))

    pins, plain = torch_requirements(torch_url)
    if not pins:
        raise SystemExit("wheels.py: torch declared no pinned CUDA dependencies, "
                         "which means the metadata format changed")
    for name in sorted(pins):
        if name in DROP:
            continue
        entries.append(from_pypi(name, pins[name]))

    out = []
    for url, sha in entries:
        out.append(url)
        # The index percent-encodes the local version (torch-2.9.1%2Bcu128);
        # pip cannot parse that as a version, so the file lands decoded.
        name = urllib.parse.unquote(url.rsplit("/", 1)[-1].split("#")[0])
        out.append(f"  out={name}")
        out.append(f"  checksum=sha-256={sha}")
    text = "\n".join(out) + "\n"

    if len(sys.argv) > 1:
        with open(sys.argv[1], "w") as f:
            f.write(text)
        # torch's non-CUDA dependencies are small and pure Python, so pip
        # installs them normally. They are listed here rather than in the
        # bootstrap so that a torch upgrade needs no second edit.
        with open(sys.argv[1] + ".deps", "w") as f:
            f.write("\n".join(plain) + "\n")
        skipped = ", ".join(sorted(n for n in pins if n in DROP)) or "none"
        print(f"wheels.py: {len(entries)} wheels to fetch, skipping {skipped}")
    else:
        sys.stdout.write(text)
        sys.stdout.write("# torch's pure-Python dependencies:\n")
        sys.stdout.write("".join(f"# {d}\n" for d in plain))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
