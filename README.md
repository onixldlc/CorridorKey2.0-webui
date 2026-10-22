# CorridorKey 2.0 — web UI

**A fork of [CorridorKey 2.0](https://github.com/nikopueringer/CorridorKey2.0)
that puts a browser front end on it, so the keying can run on a machine that
is not yours.**

## What this is, and what it is not

This is one job, done properly:

```
video in  →  green screen removed  →  video out
```

That is the whole scope. **It is not trying to be another compositor.** There
is no node graph, no timeline, no colour pipeline, no layers — if you want
those, you already have software you like, and this exists to hand that
software a clean matte. Upload a plate, key it, pull the result back out.

You can also bring your own masks: a first-frame matte or a full hint video is
an optional input to the key, and anything you upload as a hint is handed
straight to the model. More of that is planned. But the core of it stays the
line above.

## Why a web UI

CorridorKey 2.0 wants an NVIDIA card — RTX 30-series or newer, 4 GB of VRAM up
to 1440p and 6 GB for 4K. Plenty of people editing video do not have one, and
the ones who do would rather not have their workstation pinned for the length
of a render.

So: put the keyer on a **rented GPU box**, drive it from a laptop over one
tunnelled port. The render is faster because the box is faster, and nothing
local is tied up while it runs. Spin the box up, key the shot, pull the export,
spin it down.

Upstream's own interfaces are a drag-and-drop launcher and an interactive
prompt — both of which assume you are sitting at the machine with the GPU in
it. This adds the case where you are not.

A single Go binary serves the UI, embeds its own assets and shells out to
upstream's `infer.py`. Nothing about the keyer is reimplemented.

```bash
# the UI, with no GPU and no weights
docker run --rm -p 8080:8080 -e CKWEB_MODE=dev ghcr.io/onixldlc/corridorkey2-webui:latest
# http://localhost:8080
```

## How this fork is laid out

| Branch | What is on it |
|---|---|
| **`master`** | This web UI, and nothing else — a Go server, its assets and its Docker image. |
| **`upstream`** | Upstream's tree alone, pinned at a commit, tracking `nikopueringer/CorridorKey2.0`. |

The two are deliberately disjoint — `master` is an orphan branch and shares no
history with upstream at all, so its log is the UI's log and nothing else. It
has no Python in it either; the keyer is fetched from `upstream` **by the image
build**, with `git clone --depth 1 --branch upstream`, and ends up at
`/opt/corridorkey`.

That indirection is the whole point of the branch. The version of CorridorKey
an image ships is whatever `upstream` points at — a ref this fork controls —
not whatever Niko's default branch happens to be on the day someone runs a
build. The provider drives `infer.py` by its flags and its output filenames,
and both can move; this is where that is pinned.

```bash
git switch upstream
git pull                             # fast-forward to upstream's latest
git log --oneline -5 upstream        # where the pin sits now
git show upstream:infer.py           # read it without leaving master
```

Rebuild the image after moving the branch and the new runtime is in it. To
build against something else for one run, without touching the branch:

```bash
docker build -f docker/Dockerfile -t ck-web \
  --build-arg CK_REPO=https://github.com/nikopueringer/CorridorKey2.0.git \
  --build-arg CK_REF=master .
```

Every image records what it was built from, and prints it at startup:

```
ck-web: CorridorKey 2.0 runtime baked into this image from
        repo https://github.com/onixldlc/CorridorKey2.0-webui.git
        ref  upstream
        sha  485f5e9ab759ead6138cc990776184b59d6ea971
```

## Running it

### Dev run — the UI alone

No GPU, no weights, no keyer. A mock keyer written in Go produces real frames
so the grid, the viewer, the scrubber, the live log and the exports all do
their actual job:

```bash
docker run --rm -p 127.0.0.1:8080:8080 -e CKWEB_MODE=dev ghcr.io/onixldlc/corridorkey2-webui:latest
```

Or straight from source — one binary, no build step for the front end, no
`node_modules`:

```bash
go build -o ck-web . && ./ck-web -mode dev
```

### On a rented GPU box

The image is self-contained: the keyer, PyTorch and the UI are all in it.
Nothing has to be installed on the box beyond a container runtime and the
NVIDIA toolkit.

```bash
docker run -d --name ck-web \
  --gpus all \
  -p 127.0.0.1:8080:8080 \
  -e CKWEB_MODE=prod \
  -e CKWEB_TOKEN=auto \
  -v /srv/ck-web:/data \
  ghcr.io/onixldlc/corridorkey2-webui:latest
```

`/data` is **not** a checkout — it holds your clips and the weights the Modules
page installs, and that is all. Replacing or upgrading the container costs
nothing, because nothing in it is yours.

Budget the disk from what actually lands on it. The image unpacks to about
9 GB. The weights are 5.08 GB — 2.54 for the keyer, 1.65 for C-RADIO, 0.40 for
MoGe-2, 0.26 for RVM. Half-float EXR output runs about 20 MB per frame across
the four stages, so the rest is yours: 40 GB leaves roughly 24 GiB of working
room, near a thousand frames. Under 20 GB the image and the weights alone do
not both fit.

Mind the unit: disk fields on rental consoles are decimal GB and `df` reports
GiB, so a 40 GB disk is 37.3 GiB.

```
/opt/corridorkey/      the keyer, from the upstream branch (in the image)
/opt/venv/             torch 2.9.1 + cu128, upstream's pinned versions
/data/ClipsForInference/   your plates and their outputs
/data/weights/, /data/vfms/   the 5.1 GB, symlinked into the checkout
```

`CKWEB_TOKEN=auto` generates an access token and prints it, with a ready-made
link, in the container log — nothing has to be invented or remembered:

```
ck-web v0.1.0 on :8080
  auth     token required
  token    6f3b9c21a4d80e57b1c2f9aa
  open     http://localhost:8080/?t=6f3b9c21a4d80e57b1c2f9aa
```

```bash
docker logs ck-web | grep open    # the link, token included
ssh -L 8080:localhost:8080 user@gpu-box
```

Open that link through the tunnel; the token is remembered in `localStorage`.
Land on the page without it and you get an in-page prompt rather than a blank
screen.

### On a console that will not run the entrypoint

Rented-box consoles offer launch modes, and the shell ones replace the image
entrypoint with their own SSH server. Pick one of those and the UI never
starts. The image ships `onstart.sh` for exactly that case: point the
console's on-start script at it and you get both.

```
/onstart.sh
```

That is a symlink to `/usr/local/bin/onstart.sh`; `/entrypoint.sh` is one to
the real entrypoint, for the same reason — a console on-start field is typed
by hand. Use `/onstart.sh` there, not `/entrypoint.sh`: the entrypoint execs
the server in the foreground and never returns, so the token never gets read
back out of the log and no login banner is written.

It backgrounds the real entrypoint, so the volume is still seeded and the
pinned keyer revision and the `torch ... cuda True` line still appear — in
`/var/log/ck-web.log` rather than the container log. It then waits for the
token, and writes a login banner that prints the externally reachable URL,
because the link the server logs says `localhost` and the console publishes
8080 on some other port.

On Vast.ai specifically, with `-p 8080:8080` in the docker options:

| Field | Value |
| --- | --- |
| Image Path:Tag | `ghcr.io/onixldlc/corridorkey2-webui` |
| Launch mode | Interactive shell server, SSH |
| On-start script | `/onstart.sh` |
| Environment | `CKWEB_TOKEN=auto`, `CKWEB_MODE=auto` |
| Container disk | 40 GB |
| Extra filters | `verified=true compute_cap>=800 cuda_vers>=12.8 gpu_ram>=12 disk_space>=40 inet_down>=400` |

Those are floors, not targets. `compute_cap>=800` is the Ampere line, which is
what "RTX 30-series or newer" means to the filter, and `gpu_ram>=12` puts an
RTX 3060 — usually the cheapest card that can run this at all — at the bottom
of the list without hiding anything larger. On 12 GB, turn on Low VRAM in the
key panel; it trades speed for fitting in the card.

`openssh-server` is in the image so those modes have an sshd to start; nothing
in the image starts it itself.

## The weights

**No weights ship in the image.** The Modules page fetches the four things the
runtime needs, into the same folders `scripts/download_weights.py` uses, so a
checkout populated either way is identical:

| Module | Publisher | Size | Lands in |
|---|---|---|---|
| **CorridorKey 2.0** | Corridor Digital | 2.66 GB | `weights/` |
| C-RADIOv4-SO400M | NVIDIA | 1.73 GB | `vfms/cradio/` |
| MoGe-2 ViT-B | Microsoft | 419 MB | `vfms/moge/` |
| RVM small | Google DeepMind | 270 MB | `vfms/rvm/` |

About **5.1 GB**, and none of it is optional — the three foundation models are
frozen backbones that run on every pass, not alternatives you pick between.
There is one keyer checkpoint and it handles green *and* blue.

Those two folders are symlinks into `/data`, so an install lands in the volume
and survives the container it was started from.

Three details the installer gets right because upstream does:

- **The foundation models are revision-pinned.** C-RADIO at `c0457f5` and
  MoGe-2 at `ca5f0e0`, because the runtime expects those exact tensors.
  Following `main` would break in a way that surfaces far from the cause.
- **C-RADIO's duplicate is skipped.** Its repo ships a half-precision
  `.pth.tar` that duplicates `model.safetensors` and is never read. Fetching
  it would waste 1.2 GB per install.
- **RVM is checksummed.** It is the one weight not hosted on Hugging Face —
  it comes from Google's storage bucket — so its published MD5 is verified and
  a bad file is deleted rather than left to fail at load time.

### Downloads go through aria2c

A single HTTP stream does not come close to filling a rented box's link, so
the image carries `aria2c` and runs it with
`--max-connection-per-server=16 --split=16` per file. Measured on one of these
objects, 20 seconds each: **one connection 198 MiB, sixteen 370 MiB.**

That 16 is aria2's own hard limit, not a tuning choice — it rejects 17 and
exits, which would fail an install rather than slow it.

`--continue=true` means a cancelled install or a restarted container resumes
the `.part` file instead of starting over. A file is only renamed into place
once aria2 exits clean and the size is non-zero, so a partial download is
never mistaken for an installed module. Without `aria2c` on PATH the server
falls back to a single-stream Go downloader and the Modules page says so.

## What the UI gives you

- Drag-and-drop plate upload
- **Key clip** with the parameters as real controls: despill strength, input
  gamma, hint trust, low-VRAM mode
- Optional hint upload — first-frame matte or a full hint video
- Live log streaming over SSE while a job runs, with cancel
- A viewer for every output — comp, matte, foreground, cutout — with a frame
  scrubber, EXR converted to PNG on the fly
- Export any stage as ProRes 4444 with the alpha intact, or H.264
- A Modules page that installs, reports and removes weights at runtime

Jobs run one at a time on the GPU; downloads have their own lane and run
alongside, so fetching 5 GB does not block a key.

### How the outputs map

`infer.py` is run with `--output_format exr` and all four outputs, which keeps
the matte and the foreground unclamped and scene-linear — the reason to use
this rather than the preview encode. Its folders are then sorted into the
layout the viewer reads:

| infer.py | Shown as |
|---|---|
| `*_alpha_exr/` | Matte |
| `*_fg_exr/` | FG |
| `*_cutout_exr/` | Processed |
| `*_checker_comp_despill*_exr/` | Comp, transcoded to PNG |

## Flags

| Flag | Env | Default | In the image |
|---|---|---|---|
| `-addr` | `CKWEB_ADDR` | `:8080` | `:8080` |
| `-mode` | `CKWEB_MODE` | `auto` — `dev` pins the mock and blocks downloads | `auto` |
| `-provider` | `CKWEB_PROVIDER` | `auto` — or `corridorkey2` / `mock` | `auto` |
| `-root` | `CKWEB_ROOT` | `/data` — the checkout | `/opt/corridorkey` |
| `-clips` | `CKWEB_CLIPS` | `<root>/ClipsForInference` | `/data/ClipsForInference` |
| `-python` | `CKWEB_PYTHON` | `<root>/venv/bin/python`, else `python3` | `/opt/venv/bin/python` |
| `-ffmpeg` | `CKWEB_FFMPEG` | `ffmpeg` | `ffmpeg` |
| `-aria2` | `CKWEB_ARIA2` | `aria2c` — `off` forces the one-stream fallback | `aria2c` |
| `-token` | `CKWEB_TOKEN` | empty — **no auth**; `auto` generates one and logs it | unset |
| `-max-upload-gb` | — | `16` | `16` |

The defaults assume a checkout on the host; the image overrides them because
there the keyer is in the image and only the data is mounted.

## Security

The token is the only gate and it is compared as a plain string over whatever
transport you give it. Bind to `127.0.0.1` and tunnel, or put a TLS proxy in
front. **Do not expose port 8080 to the internet.** With `-token` empty every
API route is open to anything that can reach the port, and those routes write
files and start GPU work.

## The image

Built by `.github/workflows/build.yml` on a push that changes `VERSION`, for
`linux/amd64` — the CUDA wheels have no arm64 build, and neither does any box
you would rent to run this.

**Cutting a release is one edit.** Put a bare number in `VERSION`, push to
`master`, and the workflow vets and tests, builds, smoke tests, pushes
`ghcr.io/onixldlc/corridorkey2-webui:vX.Y.Z` plus a rolling `:latest`, then tags
the commit and opens a GitHub release at that tag. The release carries GitHub's
own source `.zip` and `.tar.gz`, so the sources are downloadable without
cloning. Nothing else has to be touched — it can be done from the web editor.

`workflow_dispatch` runs the same thing with a version override, and an unchecked
**publish** box builds and smoke tests without pushing or tagging anything.

CI asserts the promise rather than trusting it: `infer.py` is in the image and
its commit is printed, `torch`, `torchvision`, `cv2`, `transformers` and
`utils3d` all import, `weights/` and `vfms/` really are links into `/data`,
`aria2c` accepts 16 connections, and the build **fails** if any
`*.safetensors`, `*.pth`, `*.ckpt`, `*.pt` or `*.gguf` over 1 MB has found its
way in. Then it boots a dev run and drives an upload, a key and an export
through the HTTP API before anything is pushed.

The image that passed those tests is the image that gets pushed — retagged,
not rebuilt.

## Licence

`LICENSE` is the **CorridorKey Licence v2.0** — CC BY-NC-SA 4.0 plus additional
terms, © Corridor Digital LLC. It comes from upstream and is unchanged.

Read it. The clauses that bear on what this fork does:

- **§1 — paid work is fine.** Processing images as part of a commercial
  project is permitted, and §1b says the images and video you produce are
  yours. Renting a GPU box to key your own shots is exactly that.
- **§2b — selling access is not.** You may not offer this as a paid inference
  service: no paywall, no subscription, no usage-based billing in front of it.
  A box you rent for yourself is fine; reselling time on it to other people is
  not. This is the clause a "host it in the cloud" tool makes easiest to trip
  over, so it is worth being blunt about.
- **§2c — derivatives must stay free.** Anything you release that builds on
  this must be available to the public free of charge. Donations and
  pay-what-you-want are allowed *provided paying nothing is always an option*.
  This repo is free and public, which is the point.
- **§3 — organisations over $20M revenue** need a purchased plugin seat per
  user. See [corridorkey.com](https://corridorkey.com).
- **§4b — the "CorridorKey" name stays prominent.** Hence the repo name and
  the title of this file.

That is a plain reading of the text, not legal advice. The foundation models
carry their own licences; those notices travel with the code they cover, on
the `upstream` branch and at `/opt/corridorkey/THIRD_PARTY_NOTICES.md` in the
image.

```bash
git show upstream:THIRD_PARTY_NOTICES.md
```

## Thanks

This is a wrapper. Everything it wraps was given away by people who did not
have to.

- **[nikopueringer](https://github.com/nikopueringer)** — especially. Niko
  released CorridorKey, and its trained weights, as a freely downloadable
  project, and did it again for 2.0. That single decision is the only reason
  this port can exist: you cannot build a web UI for a model you are not
  allowed to download. Releasing the weights publicly is not the default in
  this field, and it should be said out loud that it was a choice.
- **[Corridor Digital](https://www.corridordigital.com/)** — for the work the
  tool came out of, and for putting it into the world rather than keeping it
  as in-house kit.
- **Zachary Harris** — who designed and trained the 2.0 model, in
  collaboration with Niko and Corridor Digital.
- **Ale Alvaro** — who led the official plugin, which is the thing to buy if
  you want this inside your NLE rather than in a browser:
  [corridorkey.com](https://corridorkey.com).

The keying, the model and the pipeline are theirs. What is here is a web
server in front of it. If this is useful to you, go look at their work first —
and if you work somewhere that can afford it, buy a seat.
