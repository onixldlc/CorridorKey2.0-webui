# The CorridorKey 2.0 web UI image.
#
#   docker build -f docker/Dockerfile -t ck-web .
#
# This repository is the web UI. The keyer itself is not in this branch: it
# lives on `upstream`, a pinned mirror of nikopueringer/CorridorKey2.0, and the
# build clones it from there. That indirection is the point — the version of
# CorridorKey this image ships is whatever `upstream` points at, a branch this
# fork controls, rather than whatever Niko's default branch happens to be on
# the day someone builds.
#
# Override the pin per build:
#   --build-arg CK_REF=<branch|tag|sha>   another ref in this repo
#   --build-arg CK_REPO=https://github.com/nikopueringer/CorridorKey2.0.git \
#   --build-arg CK_REF=master             track upstream directly instead
#
# The result is self-contained: runtime, PyTorch and the UI in one image. Only
# the weights (~5.1 GB) and the clips live in the volume at /data, because
# those are what must survive a container being replaced.

ARG BASE_IMAGE=docker.io/nvidia/cuda:12.8.1-base-ubuntu24.04
ARG CK_REPO=https://github.com/onixldlc/CorridorKey2.0-webui.git
ARG CK_REF=upstream
ARG TORCH_VERSION=2.9.1
ARG TORCHVISION_VERSION=0.24.1
ARG TORCH_INDEX_URL=https://download.pytorch.org/whl/cu128


# The keyer, from this fork's pinned mirror branch.
FROM docker.io/alpine/git:latest AS cksrc

ARG CK_REPO
ARG CK_REF

RUN git clone --depth 1 --branch "${CK_REF}" "${CK_REPO}" /ck

# A ref that does not carry the runtime is a build-time mistake, not a
# mysterious "provider not ready" at 3am on a rented box.
RUN test -f /ck/infer.py

RUN printf 'repo %s\nref  %s\nsha  %s\n' \
    "${CK_REPO}" "${CK_REF}" "$(git -C /ck rev-parse HEAD)" > /ck/.ck2-source-revision

RUN rm -rf /ck/.git


# Built on the host arch and cross-compiled, so a foreign-arch image does not
# need an emulated toolchain.
FROM --platform=${BUILDPLATFORM} docker.io/library/golang:1.23-alpine AS build

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ck-web .


FROM ${BASE_IMAGE}

ARG TORCH_VERSION
ARG TORCHVISION_VERSION
ARG TORCH_INDEX_URL

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update

# aria2 is what makes 5 GB of weights land in minutes instead of hours: one
# HTTP stream to Hugging Face does not come close to filling a rented box's
# link, and ck-web shells out to aria2c for 16 connections per file.
#
# git is here for requirements.txt, which pulls utils3d from a git URL.
RUN apt-get install -y --no-install-recommends \
    ca-certificates ffmpeg aria2 git python3 python3-venv

RUN rm -rf /var/lib/apt/lists/*

COPY --from=cksrc /ck /opt/corridorkey

COPY --from=build /out/ck-web /usr/local/bin/ck-web

COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/entrypoint.sh

# For launch modes that replace the entrypoint with a shell server, so the UI
# can be started alongside it rather than instead of it.
COPY docker/onstart.sh /usr/local/bin/onstart.sh

RUN chmod +x /usr/local/bin/onstart.sh

# Two commands rather than two paths to remember: what is it doing, and what
# link do I open. The log one matters most on a rented box, where a server
# that prints nowhere visible is indistinguishable from a server that hung.
COPY docker/ck-logs.sh /usr/local/bin/ck-logs

COPY docker/ck-url.sh /usr/local/bin/ck-url

RUN chmod +x /usr/local/bin/ck-logs /usr/local/bin/ck-url

# torch and its CUDA libraries are not baked in. They are 3.3 GB that would
# otherwise be re-pulled for every release of this UI, in one unsplittable
# layer, and thrown away with the container. bootstrap.sh puts them in the
# volume instead, fetched with aria2c and kept across image upgrades — the
# same bargain the weights already make.
COPY docker/bootstrap.sh /usr/local/bin/bootstrap.sh

COPY docker/wheels.py /usr/local/bin/wheels.py

RUN chmod +x /usr/local/bin/bootstrap.sh /usr/local/bin/wheels.py

# Rental consoles have a one-line field for this and no tab completion, so
# both scripts answer to a short path at the root as well.
RUN ln -s /usr/local/bin/entrypoint.sh /entrypoint.sh

RUN ln -s /usr/local/bin/onstart.sh /onstart.sh

# The runtime reads its weights from <root>/weights and <root>/vfms, and those
# are the only parts of the checkout that are written to. They are also the
# 5.1 GB nobody wants to fetch twice, so they are symlinks into the volume.
RUN mkdir -p /data/ClipsForInference /data/weights /data/wheels \
    /data/vfms/cradio /data/vfms/moge /data/vfms/rvm

RUN rm -rf /opt/corridorkey/weights /opt/corridorkey/vfms

RUN ln -s /data/weights /opt/corridorkey/weights

RUN ln -s /data/vfms /opt/corridorkey/vfms

VOLUME ["/data"]

ENV CKWEB_ADDR=":8080"

# Root is the checkout, which is in the image. Clips and weights are not.
ENV CKWEB_ROOT="/opt/corridorkey"

ENV CKWEB_CLIPS="/data/ClipsForInference"

# The interpreter lives in the volume, built on first start by bootstrap.sh.
ENV CKWEB_VENV="/data/venv"

ENV CKWEB_PYTHON="/data/venv/bin/python"

# Read by bootstrap.sh and wheels.py at run time, not at build time. The CUDA
# pins are not listed anywhere: they are read from torch's own metadata.
ENV TORCH_VERSION="${TORCH_VERSION}"

ENV TORCHVISION_VERSION="${TORCHVISION_VERSION}"

ENV TORCH_INDEX_URL="${TORCH_INDEX_URL}"

ENV CKWEB_MODE="auto"

ENV OPENCV_IO_ENABLE_OPENEXR="1"

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
