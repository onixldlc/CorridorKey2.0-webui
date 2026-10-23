# CorridorKey 2.0 — web UI

**A fork of [CorridorKey 2.0](https://github.com/nikopueringer/CorridorKey2.0)
that puts a browser front end on it, so the keying can run on a machine that
is not yours.**

## What this is, and what it is not

This is one job, done properly:

```
plate in (+ an optional rough mask)  →  refined matte out
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

### Rootless podman without a systemd user session

If `podman` answers with `crun: sd-bus call: Interactive authentication
required` and `Failed to connect to bus: /run/user/1000/bus: connection
refused`, the user’s `systemd --user` manager is not running — common on a box
reached over SSH where the account has no lingering session. podman defaults to
the systemd cgroup manager, which needs that bus, so every container fails to
start. Either fix the session:

```bash
sudo loginctl enable-linger "$USER"   # then log out and back in
```

or tell podman not to use systemd, which needs no root:

```bash
mkdir -p ~/.config/containers
printf '[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
'   >> ~/.config/containers/containers.conf
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

`/data` is **not** a checkout — it holds your clips, the weights the box
fetches for itself, and the Python stack the keyer runs on. Replacing or
upgrading the container costs nothing, because nothing in it is yours.

**The image carries no torch.** It is the UI, the keyer and ffmpeg: a few
hundred megabytes that pull in seconds. On first start ck-web queues
`bootstrap.sh`, which fetches torch and its CUDA libraries into `/data/venv`
with aria2c, at the full speed of the box's link rather than pip's one stream,
and stamps what it built. Every later start — including after an image upgrade
— matches the stamp and skips straight to the server. Four of torch's
dependencies are left out, because nothing in the keyer reaches them: `nccl`
and `nvshmem` are multi-GPU collectives and this runs on one card, `cusparselt`
is unreferenced, and `triton` only matters to `torch.compile`, which the
runtime never calls. That is 863 MiB saved on every fetch.

Budget the disk from what actually lands on it:

```
  image                 ~0.4 GB compressed, under 1 GB unpacked
  /data/venv            ~8 GiB    torch + the CUDA libraries, unpacked
  /data/wheels          ~3.3 GB   the cache, kept so a rebuild is instant
  /data/weights, vfms    4.73 GiB 2.54 keyer, 1.65 C-RADIO, 0.40 MoGe-2, 0.26 RVM
  /data/ClipsForInference  yours   about 20 MB per frame across the four stages
```

So 40 GB leaves roughly 21 GiB of working room, several hundred frames. Set
`CKWEB_KEEP_WHEELS=0` to drop the cache once the venv is built and get 3.3 GB
back, at the cost of a full refetch if the venv is ever rebuilt.

The payoff only lands if `/data` is a real volume. Point it at a host
directory or a named volume and the 3.3 GB is paid once per box; leave it on
the container's own ephemeral disk and every rent pays again.

Mind the unit: disk fields on rental consoles are decimal GB and `df` reports
GiB, so a 40 GB disk is 37.3 GiB.

```
/opt/corridorkey/      the keyer, from the upstream branch (in the image)
/data/venv/            torch + cu128, built on first start, kept across upgrades
/data/wheels/          the wheel cache aria2c filled, and the stamp
/data/ClipsForInference/   your plates and their outputs
/data/weights/, /data/vfms/   the 4.73 GiB, symlinked into the checkout
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

### It sets itself up

Nobody rents a GPU box to click four Install buttons. At startup ck-web looks
at the machine it is on, and if it looks like a production box it provisions
itself — no page, no button, nothing to remember:

1. **Is there a CUDA GPU?** `nvidia-smi` is asked. Without one `infer.py`
   refuses to run at all, so fetching ten gigabytes would be waste.
2. **Will it all fit?** The inference stack, the missing weights and room to
   key into are summed and compared with the free space on the filesystems
   that will actually hold them. On a volume that cannot finish, the download
   would die halfway and leave the box in a worse state than not starting.

If both hold, the inference stack and every missing weight are queued on the
net lane — the stack first, then the checkpoints, one at a time so they do not
fight each other for the link — and the GPU lane stays free. The whole thing
streams into the browser log like any other job. When it finishes, the
provider flips from the mock keyer to the real one on its own; the container
does not need a restart.

If either gate fails, nothing is downloaded and the Modules page says exactly
which one and by how much, with a **Set this box up** button that asks again
once you have fixed it. `POST /api/provision` is the same thing without a
browser, and `GET /api/provision` is the decision as JSON.

Two escape hatches. `-mode manual` turns self-provisioning off and leaves
every download to the buttons. `-mode prod` is the operator asserting this is
a GPU box when `nvidia-smi` is not there to say so, which skips the first gate
but not the second.

Already-provisioned boxes cost nothing: the weights are each marked by a file
on disk, the stack by the stamp `bootstrap.sh` writes, so a restart reads four
paths and does nothing.

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
  gamma, hint trust, low-VRAM mode, fit-to-VRAM
- Optional hint upload — first-frame matte or a full hint video
- Live log streaming over SSE while a job runs, with cancel, and a progress
  bar that follows the key through its four phases
- A viewer with four tabs and a frame scrubber — the plate, the rough mask
  that went in with it, the refined matte, and a preview of the subject over a
  checkerboard or a flat colour — with EXR converted to PNG on the fly
- Export any stage as ProRes 4444 with the alpha intact, or H.264
- A Modules page that installs, reports and removes weights at runtime

Jobs run one at a time on the GPU; downloads have their own lane and run
alongside, so fetching 5 GB does not block a key.

### It sizes the GPU window itself

The runtime picks its own Hann window from free VRAM, and its margin is thin:
a 3840x2160 plate on a 24 GB card estimates a 20.8 GB peak for a 24-frame
window, clears a 23.3 GB budget by 140 MB, runs for twenty minutes and then
dies on `tried to allocate 2.24 GiB`.

So ck-web makes that choice before it starts the job. It reads free VRAM from
`nvidia-smi` — the same figure the runtime computes for itself, measured
before anything is loaded — probes the plate with `ffprobe`, and runs
upstream’s own measured peak table (`corridorkey/runtime.py`’s `_VRAM_PEAK_GB`,
transcribed in `vram.go`) against a wider margin: 1.35x the estimate plus
0.5 GB, instead of 1.1x plus 0.3 GB. The window it picks is passed as an
explicit `--hann_chunk`, which turns the runtime’s own selection off. On that
same 3090 the clip above now runs a full-precision 8-frame window at a 13.9 GB
peak instead of failing.

The first line of every key says what it decided:

```
VRAM: 3840x2160 plate (3840x2176 to the model, 8.36 MP) with 23.3 GB free:
      hann_chunk=8 stride=4 full, peak ~13.9 GB of 23.3 GB
VRAM: a 24-frame window — steadier edges over time — would need the plate at
      2560x1440 on this card
```

A longer window holds the matte together over more frames, so when the native
plate only earns a short one, the size that would have earned a longer one is
named rather than left to be guessed.

When not even a single-frame window fits — a small card, or a 4K plate on
8 GB — **Fit to VRAM** (on by default) resizes the plate with Lanczos to the
largest size that does fit and keys that, saying so in the log:

```
not enough VRAM for the native plate — resizing it to 2688x1512 first.
The matte comes back at 2688x1512, so scale it up in comp, or key this clip
on a bigger card.
```

The resized copy lands in `<clip>/.fit/<w>x<h>.mp4` as 4:4:4 x264 at CRF 10 —
not a preview encode: chroma subsampling would be discarded exactly at the
green edge the matte is pulled from — and is reused on the next run. With the
toggle off, such a clip is refused up front with the resolution that would
have worked, rather than running for twenty minutes first.

### It says how far along it is

`infer.py` prints one line per Hann window and nothing at all during the two
prepasses, which at 4K is twenty silent minutes. The prepasses do leave a
trail: both fill numpy memmaps that are created at full size and written front
to back, so the blocks the filesystem has allocated under them are an exact
measure of their progress. ck-web points `--temp_dir` at `<clip>/Output/.v2tmp`
for that reason — which also keeps several GB of scratch off the container’s
overlay — and watches it, which gives four phases:

| Step | Phase | Measured by |
|---|---|---|
| 1 | loading the model | nothing — the bar pulses |
| 2 | RVM prepass | blocks written into `*_rvm_feat.f16` |
| 3 | foundation prepass | blocks written into `*_foundation_{moge,cradio}.f32` |
| 4 | keying | `window i/N`, with the frame it has reached |

Each phase carries its own percentage and a rough time left, extrapolated from
how long it has taken so far. Bar updates arrive as their own SSE event rather
than as a job change, so the console keeps the scroll position you left it at.
A killed run’s scratch is cleared at the start of the next one; a clean run
removes its own.

### One output, and a preview of it

`infer.py` is run with `--outputs alpha --output_format exr`: the matte, and
nothing else. It can also render a foreground, a cutout and a checkerboard
comp, and earlier versions of this UI asked for all four — three extra EXR
sequences per clip, more GPU time, and pictures nobody delivers. EXR keeps the
matte unclamped and scene-linear, which is the reason to run this rather than
the drag-and-drop launcher.

| infer.py | Shown as | Where it comes from |
|---|---|---|
| `*_alpha_exr/` | Matte | moved to `Output/Matte/` |
| — | Plate | the file you uploaded |
| — | Hint | `AlphaHint/`, if you uploaded one |
| — | Preview | rendered on request, never written |

The preview is the one piece of composition here, and it is built in Go rather
than asked of the model: ffmpeg hands over the plate frame and the matte frame
as PNG, and `comp.go` blends them over a checkerboard, black, white, grey,
green or magenta. The plate is scaled to the matte, not the other way around —
a plate that was resized to fit the card produced a smaller matte, and the
matte is the thing being judged. Nothing lands on disk, and the scrubber
re-renders as you drag.

The **Despill** slider moved with it. Upstream applies despill to the
foreground and to its own comp, never to the matte, so with `--outputs alpha`
the flag changed nothing on disk and is no longer passed to `infer.py`. The
slider now drives the preview instead, with the same arithmetic transcribed
from `_despill_average_srgb_torch`: the green above the mean of red and blue is
spill, and half of it goes back to the other two channels.

**Export** gives the matte as ProRes 4444 or H.264, and the preview as H.264
over whichever background is selected. The export is one ffmpeg run —
`alphamerge` + `overlay`, with the despill as a `geq` — not six hundred round
trips through Go. Both paths were checked against each other on a flat
`0x33cc33` plate: the preview returns `(129, 51, 126)` at full despill and the
export `(126, 50, 124)`, the difference being h264.

## Flags

| Flag | Env | Default | In the image |
|---|---|---|---|
| `-addr` | `CKWEB_ADDR` | `:8080` | `:8080` |
| `-mode` | `CKWEB_MODE` | `auto` — `dev` pins the mock and blocks downloads, `manual` turns off self-provisioning, `prod` provisions without a GPU check | `auto` |
| `-provider` | `CKWEB_PROVIDER` | `auto` — or `corridorkey2` / `mock` | `auto` |
| `-root` | `CKWEB_ROOT` | `/data` — the checkout | `/opt/corridorkey` |
| `-clips` | `CKWEB_CLIPS` | `<root>/ClipsForInference` | `/data/ClipsForInference` |
| `-python` | `CKWEB_PYTHON` | `<root>/venv/bin/python`, else `python3` | `/data/venv/bin/python` |
| `-ffmpeg` | `CKWEB_FFMPEG` | `ffmpeg` | `ffmpeg` |
| `-aria2` | `CKWEB_ARIA2` | `aria2c` — `off` forces the one-stream fallback | `aria2c` |
| `-bootstrap` | `CKWEB_BOOTSTRAP` | `/usr/local/bin/bootstrap.sh` — builds the inference stack | same |
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
its commit is printed, `bootstrap.sh` and `wheels.py` are present and
executable, `wheels.py` resolves the stack to real URLs with checksums and
reports the four packages it drops, `weights/` and `vfms/` really are links
into `/data`, `aria2c` accepts 16 connections, and the build **fails** if any
`*.safetensors`, `*.pth`, `*.ckpt`, `*.pt` or `*.gguf` over 1 MB has found its
way in — or if torch has, since it is not supposed to be in the image at all. Then it boots a dev run and drives an upload, a key and an export
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
