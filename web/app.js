// CorridorKey Web — the browser front for a box that has the GPU.
// No framework: the state is small enough that a render() per page is clearer
// than a dependency.

const $ = (sel) => document.querySelector(sel);

const state = {
  page: "clips",
  clips: [],
  jobs: [],
  modules: [],
  providers: [],
  selected: null,   // clip name
  view: "matte",    // viewer tab
  bg: "checker",    // what the preview composites over
  frames: [],       // frame names of the current view
  frame: 0,
  job: null,        // job id shown in the console
  log: [],
  health: null,
};

// ── token ────────────────────────────────────────────────────────────────────
// The server takes it as a header for fetches, and as ?t= for <img> and the
// event stream, which cannot carry headers.

let token = new URL(location.href).searchParams.get("t") || "";
try {
  token = token || localStorage.getItem("ck-token") || "";
  if (token) localStorage.setItem("ck-token", token);
} catch {} // private windows and blocked site data: the gate covers it

function withToken(url) {
  if (!token) return url;
  const u = new URL(url, location.origin);
  u.searchParams.set("t", token);
  return u.pathname + u.search;
}

async function api(path, opts = {}) {
  const headers = Object.assign({}, opts.headers);
  if (token) headers["X-CK-Token"] = token;
  if (opts.body && !(opts.body instanceof FormData)) headers["Content-Type"] = "application/json";
  const res = await fetch(path, Object.assign({}, opts, { headers }));
  if (res.status === 401) {
    // Never prompt(): it blocks the main thread, so the page is left
    // half-painted behind the dialog. Ask in the page instead.
    await askForToken(token ? "That token was not accepted." : "");
    return api(path, opts);
  }
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
  const ct = res.headers.get("Content-Type") || "";
  return ct.includes("json") ? res.json() : res.text();
}

// askForToken shows the gate and resolves once a token has been entered. The
// token is kept in localStorage, so it is asked for once per browser.
function askForToken(note) {
  return new Promise((resolve) => {
    const gate = $("#gate");
    const input = $("#gate-input");
    const form = $("#gate-form");
    $("#gate-note").textContent = note || "";
    $("#gate-note").hidden = !note;
    gate.hidden = false;
    input.value = "";
    input.focus();
    form.onsubmit = (e) => {
      e.preventDefault();
      const t = input.value.trim();
      if (!t) return;
      token = t;
      try {
        localStorage.setItem("ck-token", token);
      } catch {} // a private window still works, it just forgets
      gate.hidden = true;
      form.onsubmit = null;
      resolve();
    };
  });
}

function toast(msg, bad = false) {
  const el = document.createElement("div");
  el.className = "toast" + (bad ? " bad" : "");
  el.textContent = msg;
  $("#toasts").append(el);
  setTimeout(() => el.remove(), bad ? 9000 : 4500);
}

// ── params, as infer.py takes them ───────────────────────────────────────────

function params() {
  return {
    linear: $("#p-gamma").dataset.on === "true",
    despill: Number($("#p-despill").value),
    despeckle: $("#p-despeckle").dataset.on === "true",
    speckSize: Number($("#p-speck").value),
    refiner: Number($("#p-refiner").value),
    hintQuality: Number($("#p-hintq").value),
    lowVram: $("#p-lowvram").dataset.on === "true",
    fitVram: $("#p-fitvram").dataset.on === "true",
  };
}

// 2.0 and the mock keyer take different knobs, so the dock only shows the
// ones the active provider actually reads.
function syncDockToProvider(id) {
  for (const f of document.querySelectorAll(".dock .field[data-for]")) {
    f.hidden = f.dataset.for !== id;
  }
  // 2.0 has no separate hint pass: a hint is an input to the key itself.
  const v2 = id === "corridorkey2";
  $("#act-alpha").hidden = v2;
  const note = $("#act-note");
  note.hidden = !v2;
  if (v2) note.textContent = "UPLOAD A HINT WITH THE CLIP — NO SEPARATE PASS";
}

function wireDock() {
  let despillTimer = null;
  $("#p-despill").addEventListener("input", (e) => {
    $("#p-despill-v").textContent = e.target.value;
    // Each step is a different picture, so it is a different cache key and a
    // different render. Wait for the slider to settle before asking for any
    // of it.
    clearTimeout(despillTimer);
    despillTimer = setTimeout(() => {
      if (state.page === "viewer" && state.view === "comp") render();
    }, 350);
  });
  $("#p-gamma").addEventListener("click", (e) => {
    const on = e.target.dataset.on !== "true";
    e.target.dataset.on = String(on);
    e.target.textContent = on ? "Linear" : "sRGB";
    e.target.classList.toggle("on", on);
  });
  $("#p-hintq").addEventListener("input", (e) => ($("#p-hintq-v").textContent = e.target.value));
  $("#p-lowvram").addEventListener("click", (e) => {
    const on = e.target.dataset.on !== "true";
    e.target.dataset.on = String(on);
    e.target.textContent = on ? "On" : "Off";
    e.target.classList.toggle("on", on);
  });
  $("#p-fitvram").addEventListener("click", (e) => {
    const on = e.target.dataset.on !== "true";
    e.target.dataset.on = String(on);
    e.target.textContent = on ? "On" : "Off";
    e.target.classList.toggle("on", on);
  });
  $("#p-despeckle").addEventListener("click", (e) => {
    const on = e.target.dataset.on !== "true";
    e.target.dataset.on = String(on);
    e.target.textContent = on ? "On" : "Off";
    e.target.classList.toggle("on", on);
    $("#p-speck").disabled = !on;
  });
  $("#act-alpha").addEventListener("click", () => submit("generate_alphas"));
  $("#act-key").addEventListener("click", () => submit("run_inference"));
}

async function submit(action) {
  if (!state.selected) return toast("Pick a clip first.", true);
  try {
    const j = await api(`/api/clips/${encodeURIComponent(state.selected)}/${action === "run_inference" ? "key" : "alpha"}`, {
      method: "POST",
      body: JSON.stringify(params()),
    });
    state.job = j.id;
    toast(`Job #${j.id} queued: ${action} on ${state.selected}`);
    go("queue");
    await Promise.all([loadJobs(), loadLog()]);
    render();
  } catch (e) {
    toast(String(e.message || e), true);
  }
}

// ── loaders ──────────────────────────────────────────────────────────────────

async function loadClips() {
  state.clips = await api("/api/clips");
  $("#n-clips").textContent = state.clips.length || "";
  if (state.selected && !state.clips.some((c) => c.name === state.selected)) state.selected = null;
}

async function loadJobs() {
  state.jobs = (await api("/api/jobs")) || [];
  const live = state.jobs.filter((j) => j.state === "running" || j.state === "queued").length;
  $("#n-jobs").textContent = live || "";
  if (state.job == null && state.jobs.length) state.job = state.jobs[state.jobs.length - 1].id;
}

async function loadModules() {
  state.modules = await api("/api/modules");
  state.providers = await api("/api/providers");
  const have = state.modules.filter((m) => m.present).length;
  $("#n-modules").textContent = `${have}/${state.modules.length}`;
}

async function loadLog() {
  if (state.job == null) return;
  const r = await api(`/api/jobs/${state.job}/log?tail=800`);
  state.log = r.lines || [];
}

async function loadFrames() {
  if (!state.selected) {
    state.frames = [];
    return;
  }
  const r = await api(`/api/clips/${encodeURIComponent(state.selected)}/frames?view=${state.view}`);
  state.frames = r.frames || [];
  if (state.frame >= state.frames.length) state.frame = Math.max(0, state.frames.length - 1);
}

async function loadHealth() {
  const h = (state.health = await api("/api/health"));
  const g = h.gpu;
  $("#gpu-stat").textContent = g && g.length
    ? `${g[0].name} · ${g[0].memUsed}/${g[0].memTotal} MiB · ${g[0].util}%`
    : "NO GPU DETECTED";
  $("#host-stat").textContent = `${h.root} · ${h.version}`;
  const badge = $("#mode-stat");
  badge.textContent = h.dev ? "DEV · MOCK KEYER" : h.provider.toUpperCase();
  badge.className = "badge " + (h.dev ? "badge-warn" : h.providerReady ? "badge-good" : "badge-bad");
  badge.title = h.providerWhy;
  $("#dev-note").hidden = !h.dev;
}

// ── pages ────────────────────────────────────────────────────────────────────

const TITLES = {
  clips: ["KEY", "Clips"],
  viewer: ["KEY", "Viewer"],
  queue: ["STUDIO", "Queue"],
  modules: ["ASSETS", "Modules"],
  about: ["ASSETS", "About"],
};

// A hint is optional in 2.0, so a clip with nothing but a plate is already
// keyable — the stage names still come from the older pipeline.
const STAGES = {
  "needs-alpha": ["badge-accent", "ready to key"],
  ready: ["badge-accent", "ready · hint"],
  keyed: ["badge-good", "keyed"],
  empty: ["badge-bad", "no plate"],
};

function render() {
  const [root, here] = TITLES[state.page];
  $("#crumb-root").textContent = root;
  $("#crumb-here").textContent = here;
  $("#page-title").textContent = here;
  document.querySelectorAll(".sidebar .row").forEach((r) =>
    r.classList.toggle("selected", r.dataset.page === state.page));

  $("#dock").hidden = state.page !== "clips" && state.page !== "viewer";
  $("#dock-target").textContent = state.selected ? state.selected.toUpperCase() : "NO CLIP SELECTED";
  $("#act-key").disabled = !state.selected;
  $("#act-alpha").disabled = !state.selected;
  syncDockToProvider((state.health || {}).provider);

  const badge = $("#page-badge");
  badge.hidden = true;

  const body = $("#body");
  body.innerHTML = "";
  ({ clips: renderClips, viewer: renderViewer, queue: renderQueue, modules: renderModules, about: renderAbout })[state.page](body);
}

function renderClips(body) {
  body.append(dropZone());
  if (!state.clips.length) {
    body.insertAdjacentHTML("beforeend",
      `<p class="hint">No clips yet. Drop a plate in — it lands in
       <code>ClipsForInference/&lt;name&gt;/Input.&lt;ext&gt;</code>, which is where the keyer looks.</p>`);
    return;
  }
  const grid = document.createElement("div");
  grid.className = "grid";
  for (const c of state.clips) {
    const [cls, label] = STAGES[c.stage] || STAGES.empty;
    const tile = document.createElement("div");
    tile.className = "tile" + (c.name === state.selected ? " selected" : "");
    const thumbView = c.preview || "";
    tile.innerHTML = `
      <div class="thumb">
        ${thumbView
          ? `<img loading="lazy" src="${frameSrc(c.name, thumbView, 0, state.bg, c.rev)}" alt="">`
          : `<span class="empty hud-label">no preview</span>`}
        <div class="corner"><span class="badge ${cls}">${label}</span></div>
        ${c.running ? '<div class="spin"></div>' : ""}
      </div>
      <div class="meta">
        <span class="name">${esc(c.name)}</span>
        <span class="line">
          <span class="hud-label">${esc(c.input || "—")}</span>
          ${c.alpha > 0 ? `<span class="hud-label">· ${c.alpha} hint</span>` : ""}
          ${c.frames > 0 ? `<span class="hud-label">· ${c.frames} matte</span>` : ""}
        </span>
      </div>
      <div class="acts">
        <button class="btn-mono" data-act="open">Open</button>
        <button class="btn-mono btn-ghost btn-danger" data-act="del">Delete</button>
      </div>`;
    tile.addEventListener("click", (e) => {
      const act = e.target.dataset.act;
      if (act === "del") return delClip(c.name);
      state.selected = c.name;
      if (act === "open") {
        state.view = c.stage === "keyed" ? "matte" : c.alpha > 0 ? "hint" : "plate";
        state.frame = 0;
        return go("viewer");
      }
      render();
    });
    grid.append(tile);
  }
  body.append(grid);
}

function dropZone() {
  const el = document.createElement("div");
  el.className = "drop";
  el.innerHTML = `
    <div class="font-display" style="font-size:18px">Drop a plate here</div>
    <div class="hud-label" style="margin-top:6px">MOV · MP4 · MKV · MXF · AVI — name a file *alphaHint* to supply your own hint</div>
    <div class="cta">
      <button class="btn-mono" id="pick">Choose file</button>
      <input type="file" id="file" accept="video/*,.mov,.mxf,.mkv" multiple hidden>
    </div>
    <div class="hud-label" id="up-stat" style="margin-top:8px"></div>`;
  el.querySelector("#pick").addEventListener("click", () => el.querySelector("#file").click());
  el.querySelector("#file").addEventListener("change", (e) => upload(e.target.files));
  el.addEventListener("dragover", (e) => { e.preventDefault(); el.classList.add("over"); });
  el.addEventListener("dragleave", () => el.classList.remove("over"));
  el.addEventListener("drop", (e) => {
    e.preventDefault();
    el.classList.remove("over");
    upload(e.dataTransfer.files);
  });
  return el;
}

// upload sends one file per request with XHR, because fetch gives no progress.
function upload(files) {
  if (!files || !files.length) return;
  const stat = $("#up-stat");
  const queueList = [...files];
  const next = () => {
    const f = queueList.shift();
    if (!f) {
      stat.textContent = "";
      refresh();
      return;
    }
    const fd = new FormData();
    fd.append("file", f, f.name);
    const xhr = new XMLHttpRequest();
    xhr.open("POST", "/api/upload");
    if (token) xhr.setRequestHeader("X-CK-Token", token);
    xhr.upload.onprogress = (e) => {
      if (!e.lengthComputable) return;
      stat.textContent = `${f.name} — ${Math.round((e.loaded / e.total) * 100)}%`;
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) toast(`Uploaded ${f.name}`);
      else toast(`${f.name}: ${xhr.responseText.trim() || xhr.statusText}`, true);
      next();
    };
    xhr.onerror = () => { toast(`${f.name}: upload failed`, true); next(); };
    xhr.send(fd);
  };
  next();
}

async function delClip(name) {
  if (!confirm(`Delete clip "${name}" and everything under it?`)) return;
  try {
    await api(`/api/clips/${encodeURIComponent(name)}`, { method: "DELETE" });
    if (state.selected === name) state.selected = null;
    toast(`Deleted ${name}`);
    refresh();
  } catch (e) {
    toast(String(e.message || e), true);
  }
}

// What this UI deals in: the plate that went in, the rough mask that went in
// with it, the refined matte that came out, and a preview of the three
// together. Nothing else — CorridorKey is a keyer, not a compositor.
const VIEWS = [
  ["plate", "Plate"],
  ["hint", "Hint"],
  ["matte", "Matte"],
  ["comp", "Preview"],
];

const EMPTY_VIEW = {
  plate: "No plate here yet — drop one on the Clips page.",
  hint: "No rough mask uploaded for this clip.",
  matte: "No matte yet.",
  comp: "No matte yet, so there is nothing to composite.",
};

const BACKGROUNDS = ["checker", "black", "white", "grey", "green", "magenta"];

// frameSrc is the URL of one frame of the current view. The preview carries
// the background with it, because the server renders it rather than reading it.
function despillNow() {
  const el = $("#p-despill");
  return el ? el.value : 5;
}

function frameSrc(clip, view, i, bg, rev) {
  // Despill belongs to the preview now: the matte does not depend on it, so
  // the server applies the slider here rather than passing it to infer.py.
  // rev is the clip's revision: it puts a re-keyed clip on new URLs, so a
  // cached frame of the old matte can never be served for the new one.
  const q = `view=${view}&i=${i}`
    + (view === "comp" ? `&bg=${bg}&despill=${despillNow()}` : "")
    + (rev ? `&v=${encodeURIComponent(rev)}` : "");
  return withToken(`/api/clips/${encodeURIComponent(clip)}/frame?${q}`);
}

// ── frames: cache first, queue of one, warm the rest ──────────────────────
//
// Every comp frame costs the server two ffmpeg calls and a blend, so dragging
// the scrubber used to fire one of those per input event and bury the box.
// Now: at most one request is in flight, the newest frame always wins, and
// everything that comes back is kept in the Cache Storage API — which is sized
// against free disk rather than localStorage's five megabytes, so a whole
// clip's worth of previews fits. Frames already in there cost no server work
// at all, not even a 304.
const FRAME_CACHE = "ck-frames-v1";
const WARM_AHEAD = 32;     // frames to fill in ahead of the cursor
const WARM_PARALLEL = 2;   // in flight while warming, so the cursor stays first

const strip = {
  key: "",                 // identity of the sequence on screen
  covered: new Set(),      // frame indexes known to be in the cache
  pending: new Set(),      // frame indexes being fetched right now
  blobs: new Map(),        // url -> object URL, revoked when the strip changes
  want: 0,                 // the frame the user is actually on
  inFlight: null,          // AbortController of the cursor's own request
  warmAbort: null,         // AbortController for everything being warmed
  dirty: new Set(),        // coverage segments whose state changed
  painted: -1,             // where the cursor marker currently is
  warmTimer: null,
  warming: false,
};

// The player. Frames come from the cache, so playback is a timer and an img
// src; a frame that has not been rendered yet holds the clock rather than
// being skipped, the way a video player waits on its buffer.
const player = { playing: false, timer: null, loop: true };

function stripKey() {
  const clip = state.clips.find((c) => c.name === state.selected) || {};
  return [state.selected, state.view, state.view === "comp" ? state.bg : "",
    state.view === "comp" ? despillNow() : "", clip.rev || ""].join("|");
}

function clipRev() {
  const clip = state.clips.find((c) => c.name === state.selected) || {};
  return clip.rev || "";
}

// frameCache is absent on an insecure origin and in some private windows, in
// which case everything below still works, just without the cache.
async function frameCache() {
  try {
    if (!window.caches) return null;
    return await caches.open(FRAME_CACHE);
  } catch {
    return null;
  }
}

// getFrame returns an object URL for one frame, from the cache when it is
// there and from the server when it is not.
async function getFrame(url, signal) {
  if (strip.blobs.has(url)) return strip.blobs.get(url);
  const cache = await frameCache();
  let res = cache ? await cache.match(url) : null;
  if (!res) {
    res = await fetch(url, { signal, headers: token ? { "X-CK-Token": token } : {} });
    if (!res.ok) throw new Error(`frame ${res.status}`);
    if (cache) {
      try { await cache.put(url, res.clone()); } catch {} // quota, or a 206
    }
  }
  const blob = await res.blob();
  const obj = URL.createObjectURL(blob);
  strip.blobs.set(url, obj);
  return obj;
}

// resetStrip drops what belonged to the previous sequence and reads back which
// frames of the new one are already cached, so the coverage bar is right
// before anything is fetched.
// stopWarming drops everything being fetched for a sequence that is no longer
// on screen. Opening another page, another clip or another view must not keep
// the box rendering frames nobody is looking at — and neither must a tab the
// browser has pushed into the background.
function stopWarming() {
  clearTimeout(strip.warmTimer);
  strip.warmTimer = null;
  if (strip.warmAbort) strip.warmAbort.abort();
  strip.warmAbort = null;
  strip.warming = false;
  strip.pending.clear();
  paintCoverage();
}

// warmingAllowed is the whole of the "only fetch for the page I am on" rule.
function warmingAllowed() {
  return state.page === "viewer" && !document.hidden && state.frames.length > 0;
}

async function resetStrip() {
  stopPlay();
  for (const obj of strip.blobs.values()) URL.revokeObjectURL(obj);
  strip.blobs.clear();
  strip.covered.clear();
  stopWarming();
  if (strip.inFlight) strip.inFlight.abort();
  strip.inFlight = null;
  strip.key = stripKey();

  const cache = await frameCache();
  if (!cache || !state.selected) return;
  const rev = clipRev();
  const prefix = `/api/clips/${encodeURIComponent(state.selected)}/frame?`;
  for (const req of await cache.keys()) {
    const u = new URL(req.url, location.origin);
    if (!u.pathname.startsWith(`/api/clips/${encodeURIComponent(state.selected)}/frame`)) continue;
    // A frame of an older key is unreachable now; drop it rather than let it
    // sit in the quota forever.
    if (rev && u.searchParams.get("v") && u.searchParams.get("v") !== rev) {
      cache.delete(req);
      continue;
    }
    if (u.searchParams.get("view") !== state.view) continue;
    if (state.view === "comp"
      && (u.searchParams.get("bg") !== state.bg
        || u.searchParams.get("despill") !== String(despillNow()))) continue;
    strip.covered.add(Number(u.searchParams.get("i")));
  }
  void prefix;
}

// showFrame puts frame i on screen. For a comp it shows the matte first,
// which is one ffmpeg call rather than three and is usually already cached,
// and swaps the composite in when it arrives — so the scrubber never waits on
// the blend of a frame the user has already dragged past.
async function showFrame(i) {
  strip.want = i;
  const img = document.querySelector(".stage img");
  if (!img) return;
  const rev = clipRev();

  // Already in hand: no request, no await, no placeholder. This is the path
  // playback runs on, and it is why playback is smooth.
  const have = strip.blobs.get(frameSrc(state.selected, state.view, i, state.bg, rev));
  if (have) {
    img.src = have;
    img.classList.remove("placeholder");
    paintCoverage();
    return;
  }

  if (state.view === "comp" && !strip.covered.has(i)) {
    const matte = frameSrc(state.selected, "matte", i, state.bg, rev);
    try {
      const obj = await getFrame(matte);
      if (strip.want === i && !strip.covered.has(i)) {
        img.src = obj;
        img.classList.add("placeholder");
      }
    } catch {} // the matte is a courtesy; the comp below is the real answer
    if (strip.want !== i) return;
  }

  if (strip.inFlight) strip.inFlight.abort();
  strip.inFlight = new AbortController();
  const url = frameSrc(state.selected, state.view, i, state.bg, rev);
  strip.pending.add(i);
  strip.dirty.add(i);
  paintCoverage();
  try {
    const obj = await getFrame(url, strip.inFlight.signal);
    strip.covered.add(i);
    strip.dirty.add(i);
    if (strip.want === i) {
      img.src = obj;
      img.classList.remove("placeholder");
    }
  } catch (e) {
    if (e.name !== "AbortError") toast(`Frame ${i + 1}: ${e.message}`, true);
  } finally {
    strip.pending.delete(i);
    strip.dirty.add(i);
    paintCoverage();
    scheduleWarm();
  }
}

// scheduleWarm fills the cache forward from the cursor once the user has
// stopped dragging. It never competes with the cursor: WARM_PARALLEL is small
// and showFrame's own request is not one of them.
function scheduleWarm(delay = 400) {
  clearTimeout(strip.warmTimer);
  if (!warmingAllowed()) return;
  strip.warmTimer = setTimeout(warm, delay);
}

async function warm() {
  if (strip.warming || !warmingAllowed()) return;
  strip.warming = true;
  strip.warmAbort = new AbortController();
  const signal = strip.warmAbort.signal;
  const mine = strip.key;
  const rev = clipRev();
  try {
    // Forward from the cursor, wrapping once, because playback loops and the
    // frames behind the cursor are the ones it comes back to.
    const todo = [];
    for (let n = 0; n < state.frames.length && todo.length < WARM_AHEAD; n++) {
      const i = (strip.want + n) % state.frames.length;
      if (!strip.covered.has(i) && !strip.pending.has(i)) todo.push(i);
    }
    while (todo.length) {
      if (strip.key !== mine || !warmingAllowed() || signal.aborted) return;
      const batch = todo.splice(0, WARM_PARALLEL);
      await Promise.all(batch.map(async (i) => {
        strip.pending.add(i);
        strip.dirty.add(i);
        paintCoverage();
        try {
          await getFrame(frameSrc(state.selected, state.view, i, state.bg, rev), signal);
          strip.covered.add(i);
        } catch {} // a warm frame that fails is not worth a toast
        strip.pending.delete(i);
        strip.dirty.add(i);
        paintCoverage();
      }));
    }
  } finally {
    strip.warming = false;
  }
}

// paintCoverage draws the bar above the frame: which of the sequence has been
// rendered and cached, which is being worked on, and where the cursor is.
// paintCoverage repaints only what changed. Playback calls this per frame, and
// a 348-frame clip has 348 segments: rewriting all of them 24 times a second
// is what held the player to half speed before.
function paintCoverage(all = false) {
  const bar = document.querySelector(".coverage");
  if (!bar) return;
  const total = state.frames.length;
  if (!total) return;

  const touch = all ? null : new Set(strip.dirty);
  if (touch) {
    if (strip.painted >= 0) touch.add(strip.painted);
    touch.add(strip.want);
  }
  strip.dirty.clear();
  strip.painted = strip.want;

  const segs = bar.children;
  const paint = (i) => {
    const seg = segs[i];
    if (!seg) return;
    seg.className = "seg"
      + (strip.covered.has(i) ? " on" : "")
      + (strip.pending.has(i) ? " busy" : "")
      + (i === strip.want ? " at" : "");
  };
  if (touch) touch.forEach(paint);
  else for (let i = 0; i < total; i++) paint(i);

  const label = bar.parentElement && bar.parentElement.querySelector(".coverage-label");
  if (label) {
    label.textContent = state.view === "comp"
      ? `${strip.covered.size} / ${total} composited`
      : `${strip.covered.size} / ${total} cached`;
  }
}

function coverageBar() {
  const wrap = document.createElement("div");
  wrap.className = "coverage-wrap";
  const bar = document.createElement("div");
  bar.className = "coverage";
  for (let i = 0; i < state.frames.length; i++) {
    const seg = document.createElement("span");
    seg.className = "seg";
    seg.dataset.i = i;
    seg.title = `frame ${i + 1}`;
    seg.addEventListener("click", () => jumpTo(i));
    bar.append(seg);
  }
  wrap.innerHTML = `<span class="hud-label coverage-label"></span>`;
  wrap.append(bar);
  return wrap;
}

// ── the player ────────────────────────────────────────────────────────

function playFPS() {
  const clip = state.clips.find((c) => c.name === state.selected) || {};
  return clip.fps > 0 ? clip.fps : 24;
}

function playToggle() {
  if (player.playing) stopPlay();
  else startPlay();
}

function startPlay() {
  if (state.frames.length < 2 || state.page !== "viewer") return;
  player.playing = true;
  paintTransport();
  scheduleWarm(0); // the player eats cache, so start filling it immediately
  tickPlay(performance.now());
}

function stopPlay() {
  player.playing = false;
  clearTimeout(player.timer);
  player.timer = null;
  paintTransport();
}

function tickPlay(due) {
  if (!player.playing) return;
  let next = strip.want + 1;
  if (next >= state.frames.length) {
    if (!player.loop) return stopPlay();
    next = 0;
  }
  // Hold the clock on a frame that has not been composited yet rather than
  // skipping it — the same thing a video player does with an empty buffer. The
  // coverage bar shows why it is waiting.
  if (!strip.covered.has(next)) {
    scheduleWarm(0);
    // Waiting on the warmer, so the clock restarts from now rather than
    // trying to catch up on frames that were never shown.
    player.timer = setTimeout(() => tickPlay(performance.now()), 100);
    return;
  }
  jumpTo(next);
  // Against the clock, not against the last frame: setTimeout always runs a
  // little late, and paying that cost every frame would drift the playback
  // slower than the plate was shot.
  const step = 1000 / playFPS();
  const target = (due || performance.now()) + step;
  const wait = Math.max(0, target - performance.now());
  player.timer = setTimeout(() => tickPlay(target), wait);
}

function stepBy(n) {
  stopPlay();
  const total = state.frames.length;
  if (!total) return;
  jumpTo((strip.want + n + total) % total);
}

function paintTransport() {
  const play = document.querySelector(".scrub .play");
  if (play) {
    play.textContent = player.playing ? "❚❚" : "▶";
    play.title = player.playing ? "Pause (space)" : "Play (space)";
    play.classList.toggle("on", player.playing);
  }
  const loop = document.querySelector(".scrub .loop");
  if (loop) loop.classList.toggle("on", player.loop);
}

// jumpTo moves the cursor without re-rendering the page, so the scrubber keeps
// its grab and the console keeps its scroll.
function jumpTo(i) {
  state.frame = i;
  const no = document.querySelector(".scrub .frame-no");
  if (no) no.textContent = `${i + 1} / ${state.frames.length} · ${playFPS().toFixed(2)} fps`;
  const range = document.querySelector(".scrub input");
  if (range && Number(range.value) !== i) range.value = i;
  paintCoverage();
  showFrame(i);
}

function renderViewer(body) {
  if (!state.selected) {
    body.innerHTML = `<p class="hint">Pick a clip on the Clips page first.</p>`;
    return;
  }
  const clip = state.clips.find((c) => c.name === state.selected) || {};
  const wrap = document.createElement("div");
  wrap.className = "viewer";

  const bar = document.createElement("div");
  bar.className = "view-bar";
  for (const [key, label] of VIEWS) {
    const b = document.createElement("button");
    b.className = "tab" + (state.view === key ? " on" : "");
    b.textContent = label;
    b.addEventListener("click", async () => {
      state.view = key;
      state.frame = 0;
      location.hash = `viewer:${encodeURIComponent(state.selected)}:${key}`;
      await loadFrames();
      render();
    });
    bar.append(b);
  }
  const sp = document.createElement("div");
  sp.style.flex = "1";
  bar.append(sp);
  const exp = (format) => withToken(`/api/clips/${encodeURIComponent(state.selected)}/export?view=${state.view}&format=${format}`
    + (state.view === "comp" ? `&bg=${state.bg}&despill=${$("#p-despill").value}` : ""));
  bar.insertAdjacentHTML("beforeend", state.view === "comp"
    ? `<a class="btn btn-mono" href="${exp("mp4")}">Export H.264</a>`
    : `<a class="btn btn-mono" href="${exp("mov")}">Export ProRes 4444</a>
       <a class="btn btn-mono" href="${exp("mp4")}">Export H.264</a>`);
  wrap.append(bar);

  if (state.view === "comp") {
    const bgs = document.createElement("div");
    bgs.className = "view-bar";
    bgs.insertAdjacentHTML("beforeend", `<span class="hud-label">Background</span>`);
    for (const name of BACKGROUNDS) {
      const b = document.createElement("button");
      b.className = "tab" + (state.bg === name ? " on" : "");
      b.textContent = name;
      b.addEventListener("click", () => {
        state.bg = name;
        render();
      });
      bgs.append(b);
    }
    wrap.append(bgs);
  }

  if (state.frames.length > 1) wrap.append(coverageBar());

  const stage = document.createElement("div");
  stage.className = "stage";
  if (state.view === "plate" && clip.input && !clip.input.endsWith("/")) {
    stage.innerHTML = `<video controls preload="metadata" src="${withToken(`/api/clips/${encodeURIComponent(state.selected)}/input`)}"></video>`;
  } else if (state.frames.length) {
    // No src: showFrame() fills it from the cache or the server, so a frame
    // that is already cached appears without a request at all.
    stage.innerHTML = `<img alt="" decoding="async">`;
  } else {
    stage.innerHTML = `<div class="empty">${esc(EMPTY_VIEW[state.view] || `Nothing in ${state.view} yet.`)}<br>
      <span class="hud-label">${state.view === "hint" ? "a hint is optional" : "key the clip to fill this"}</span></div>`;
  }
  wrap.append(stage);

  if (state.frames.length > 1) {
    const scrub = document.createElement("div");
    scrub.className = "scrub";
    scrub.innerHTML = `
      <button class="tab step" title="Previous frame (left arrow)">⏪</button>
      <button class="tab play" title="Play (space)">▶</button>
      <button class="tab step fwd" title="Next frame (right arrow)">⏩</button>
      <button class="tab loop" title="Loop">↺</button>
      <input type="range" min="0" max="${state.frames.length - 1}" value="${state.frame}">
      <span class="frame-no">${state.frame + 1} / ${state.frames.length} · ${playFPS().toFixed(2)} fps</span>`;
    scrub.querySelector("input").addEventListener("input", (e) => {
      stopPlay(); // a hand on the scrubber wins over the clock
      jumpTo(Number(e.target.value));
    });
    scrub.querySelector(".play").addEventListener("click", playToggle);
    scrub.querySelector(".step").addEventListener("click", () => stepBy(-1));
    scrub.querySelector(".step.fwd").addEventListener("click", () => stepBy(1));
    scrub.querySelector(".loop").addEventListener("click", () => {
      player.loop = !player.loop;
      paintTransport();
    });
    wrap.append(scrub);
    paintTransport();
  }
  body.append(wrap);

  if (state.frames.length) {
    const go = () => { paintCoverage(true); showFrame(state.frame); };
    if (strip.key !== stripKey()) resetStrip().then(go);
    else go();
  }
}

// progressHTML draws the bar a running key reports. A phase with no number
// behind it — the model loading, or a prepass on a filesystem that does not
// do sparse files — gets a bar that pulses instead of one that lies.
function progressHTML(j) {
  const p = j.progress;
  if (!p || j.state !== "running") return "";
  const known = p.pct >= 0;
  const bits = [p.phase, `step ${p.step} of ${p.steps}`];
  if (known) bits.push(`${p.pct}%`);
  if (p.note) bits.push(p.note);
  return `<div class="prog">
    <div class="prog-bar${known ? "" : " unknown"}"><span style="width:${known ? p.pct : 100}%"></span></div>
    <span class="hud-label">${esc(bits.join(" · "))}</span>
  </div>`;
}

// applyProgress moves one bar in place. A full re-render would also work and
// would also scroll the console back to the bottom every second.
function applyProgress(id, p) {
  const j = state.jobs.find((x) => x.id === id);
  if (j) j.progress = p;
  const row = document.querySelector(`.job[data-id="${id}"]`);
  if (!row || !j) return;
  const html = progressHTML(j);
  const bar = row.querySelector(".prog");
  if (bar) bar.outerHTML = html;
  else if (html) row.insertAdjacentHTML("beforeend", html);
}

function renderQueue(body) {
  const jobs = document.createElement("div");
  jobs.className = "jobs";
  if (!state.jobs.length) {
    jobs.innerHTML = `<p class="hint">Nothing has run yet.</p>`;
  }
  for (const j of [...state.jobs].reverse()) {
    const row = document.createElement("div");
    row.className = "job" + (j.id === state.job ? " selected" : "");
    row.dataset.id = j.id;
    const cls = { done: "badge-good", failed: "badge-bad", running: "badge-accent", cancelled: "badge", queued: "badge-warn" }[j.state] || "badge";
    row.innerHTML = `
      <span class="id">#${j.id}</span>
      <span class="badge ${cls}">${j.state}</span>
      <span class="mono-label" style="color:var(--muted)">${
        { run_inference: "KEY", generate_alphas: "ALPHA", install: "FETCH", bootstrap: "SETUP" }[j.action] || esc(j.action)}</span>
      <span>${esc(j.clip || j.module || (j.action === "bootstrap" ? "inference stack" : "all clips"))}</span>
      ${j.provider ? `<span class="hud-label">via ${esc(j.provider)}</span>` : ""}
      <div class="spacer"></div>
      ${j.state === "running" || j.state === "queued" ? `<button class="btn-mono btn-ghost btn-danger" data-cancel="${j.id}">Cancel</button>` : ""}
      ${progressHTML(j)}`;
    row.addEventListener("click", async (e) => {
      if (e.target.dataset.cancel) {
        await api(`/api/jobs/${e.target.dataset.cancel}/cancel`, { method: "POST" });
        return;
      }
      state.job = j.id;
      await loadLog();
      render();
    });
    jobs.append(row);
  }
  body.append(jobs);

  const con = document.createElement("div");
  con.className = "console";
  con.id = "console";
  con.innerHTML = state.log.length
    ? state.log.map(logLine).join("\n")
    : `<span class="hud-label">no output</span>`;
  body.append(con);
  con.scrollTop = con.scrollHeight;
}

function logLine(l) {
  const cls = /error|traceback|failed|cuda out of memory/i.test(l) ? "err" : /done|complete|saved/i.test(l) ? "ok" : "";
  return cls ? `<span class="${cls}">${esc(l)}</span>` : esc(l);
}

function renderModules(body) {
  const h = state.health || {};

  const provs = document.createElement("div");
  provs.innerHTML = `<div class="section-head"><span class="mono-label">Providers</span></div>`;
  for (const p of state.providers) {
    const row = document.createElement("div");
    row.className = "job" + (p.active ? " selected" : "");
    row.innerHTML = `
      <span class="badge ${p.active ? "badge-accent" : ""}">${p.active ? "active" : "idle"}</span>
      <span style="font-weight:500">${esc(p.title)}</span>
      <span class="hud-label">${esc(p.detail)}</span>
      <div class="spacer"></div>
      <span class="badge ${p.ready ? "badge-good" : "badge-bad"}">${p.ready ? "usable" : "not here"}</span>`;
    provs.append(row);
  }
  provs.insertAdjacentHTML("beforeend", `
    <p class="hint" style="margin-top:10px">A provider is whatever does the keying. Pick one at startup with
    <code>-provider</code>; <code>auto</code> uses the real keyer when the checkout and its checkpoint are
    present and the mock otherwise. <code>-mode dev</code> pins the mock and blocks every download, and <code>-mode manual</code>
    leaves every download to the buttons below.</p>`);
  body.append(provs);

  body.insertAdjacentHTML("beforeend", `
    <div class="section-head" style="margin-top:22px"><span class="mono-label">Modules</span></div>
    <p class="hint">No weights ship in this image — between them they are tens of gigabytes. Install the ones
    a run needs; they land in <code>${esc(h.root || "")}</code> on the mounted volume and survive restarts.
    ${h.dev ? '<b>This is a dev run, so installing is off.</b> The mock keyer needs no model.' : ""}</p>
    ${h.dev ? "" : (h.fetcher && h.fetcher !== "go"
      ? `<p class="hint">Fetched with <code>${esc(h.fetcher)}</code> — several connections per file, and a
         cancelled install resumes where it stopped.</p>`
      : `<p class="hint"><span class="badge badge-warn">slow</span> No <code>aria2c</code> on PATH, so each
         file comes down over one connection. Install it on the host, or use the image, to saturate the link.</p>`)}`);

  renderProvision(body);

  const t = document.createElement("table");
  t.className = "models";
  t.innerHTML = `<thead><tr>
      <th>Module</th><th>Status</th><th>Size</th><th>Lands in</th><th>Source</th><th></th>
    </tr></thead><tbody></tbody>`;
  const tb = t.querySelector("tbody");
  for (const m of state.modules) {
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>
        <div>${esc(m.title)}${m.required ? ' <span class="badge badge-accent">required</span>' : ""}</div>
        <div class="hud-label" style="margin-top:4px">${esc(m.note || "")}</div>
      </td>
      <td><span class="badge ${m.installing ? "badge-warn" : m.present ? "badge-good" : "badge-bad"}">${
        m.installing ? "installing" : m.present ? "on disk" : "missing"}</span></td>
      <td class="hud-label">${m.present ? fmtBytes(m.bytes) : esc(m.size)}</td>
      <td><code>${esc(m.dest)}</code></td>
      <td><a href="${esc(m.source)}" target="_blank" rel="noreferrer noopener">${esc(hostOf(m.source))}</a></td>
      <td style="text-align:right;white-space:nowrap">
        ${m.present
          ? `<button class="btn-mono btn-ghost btn-danger" data-rm="${esc(m.key)}">Remove</button>`
          : `<button class="btn-mono" data-install="${esc(m.key)}" ${h.dev || m.installing ? "disabled" : ""}>Install</button>`}
      </td>`;
    tb.append(tr);
  }
  t.addEventListener("click", async (e) => {
    const inst = e.target.dataset.install;
    const rm = e.target.dataset.rm;
    try {
      if (inst) {
        const j = await api(`/api/modules/${inst}/install`, { method: "POST" });
        state.job = j.id;
        toast(`Job #${j.id}: downloading ${inst}`);
        go("queue");
        await Promise.all([loadJobs(), loadLog()]);
        return render();
      }
      if (rm) {
        if (!confirm(`Remove ${rm} from disk?`)) return;
        await api(`/api/modules/${rm}`, { method: "DELETE" });
        toast(`Removed ${rm}`);
        await loadModules();
        render();
      }
    } catch (err) {
      toast(String(err.message || err), true);
    }
  });
  body.append(t);
}

// renderProvision explains the one thing an operator on a rented box should
// not have to work out from four missing rows: whether this machine decided
// it is a production box, and if not, what stopped it.
function renderProvision(body) {
  const h = state.health || {};
  const p = h.provision || {};
  const disks = (p.disks || [])
    .map((d) => `<div class="hud-label">${esc(d.path)} — ${fmtBytes(d.need)} wanted for ${esc(d.what)}, ${
      d.free < 0 ? "free space unknown" : `${fmtBytes(d.free)} free`}</div>`)
    .join("");

  const box = document.createElement("div");
  box.className = "hint";
  box.setAttribute("style", "display:flex;flex-direction:column;gap:8px;margin:12px 0");
  box.innerHTML = `
    <div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">
      <span class="badge ${p.auto ? "badge-good" : "badge-warn"}">${p.auto ? "automatic" : "manual"}</span>
      <span>${esc(p.reason || "setup has not been decided yet")}</span>
    </div>
    <div class="hud-label">${p.gpus || 0} GPU(s) · inference stack ${p.stack ? "built" : "not built"} · ${
      (p.missing || []).length} weight(s) missing${p.jobs && p.jobs.length ? ` · jobs ${p.jobs.join(", ")}` : ""}</div>
    ${disks}
    <div><button class="btn-mono" id="provision-now" ${h.dev ? "disabled" : ""}>Set this box up</button></div>`;

  box.querySelector("#provision-now").addEventListener("click", async (e) => {
    e.target.disabled = true;
    try {
      const r = await api("/api/provision", { method: "POST" });
      toast(r.reason || "", !r.auto);
      await Promise.all([loadHealth(), loadModules(), loadJobs()]);
      if (r.jobs && r.jobs.length) {
        state.job = r.jobs[0];
        go("queue");
        await loadLog();
      }
      render();
    } catch (err) {
      toast(String(err.message || err), true);
      e.target.disabled = false;
    }
  });
  body.append(box);
}

function renderAbout(body) {
  const h = state.health || {};
  const gpu = (h.gpu && h.gpu.length) ? h.gpu.map((g) => g.name).join(", ") : "none detected";
  const weights = state.modules.filter((m) => m.required);
  const have = weights.filter((m) => m.present).length;

  body.insertAdjacentHTML("beforeend", `
    <div class="hint" style="display:flex;flex-direction:column;gap:12px">
      <p><b>ck-web</b> is a browser front end for
      <a href="https://github.com/nikopueringer/CorridorKey2.0" target="_blank" rel="noreferrer noopener">CorridorKey 2.0</a>
      by Corridor Digital. It does not reimplement the keyer: every key here shells out to the
      project's own <code>infer.py</code> and reads back what that writes.</p>

      <p>One job, done properly — <b>a plate, and an optional rough mask, in; a refined matte out.</b>
      It is not a compositor. There is no node graph, no timeline and no colour pipeline; this exists
      to hand the software you already use a clean matte.</p>

      <p>What that looks like on disk:</p>
      <ol>
        <li><code>ClipsForInference/&lt;clip&gt;/Input.&lt;ext&gt;</code> — the plate you upload.</li>
        <li><code>AlphaHint/</code> — optional. 2.0 has no separate hint pass: a hint is an input to the
        key itself, passed as <code>--hint_video</code>. Name an upload <i>alphaHint</i> to supply one.</li>
        <li><code>Output/Matte/</code> — the matte, as half-float EXR, unclamped and scene-linear.
        <code>infer.py</code> is asked for <code>--outputs alpha</code> and nothing else.</li>
      </ol>

      <p>The <b>Preview</b> tab is the only composition here, and it is built by this server rather than
      by the model: the plate frame and the matte frame are blended over a checkerboard or a flat colour
      on request, with the <i>Despill</i> slider applied as upstream applies it. Nothing is written to disk
      for it. Browsers cannot read EXR, so every frame you see went through ffmpeg on the way.</p>

      <p>Keying is done by a <b>provider</b>, and the weights it needs are <b>modules</b> fetched at runtime —
      the image carries the code and the setup machinery, not the 5.1 GB of checkpoints or the 3.3 GB of
      PyTorch. On a box with a CUDA GPU and the disk to finish, ck-web provisions itself at startup: the
      inference stack first, then every missing weight, all on the download lane so the GPU is never held
      up by a fetch. It also sizes the GPU window itself from a measured table before each key, and will
      resize a plate that cannot fit the card rather than running out of memory twenty minutes in.</p>

      <p>The <i>mock</i> provider keys with a plain green-excess formula in Go. It is enough to exercise
      every screen here without a GPU, a Python install or a single downloaded byte — and it writes a
      matte only, like the real one.</p>

      <p>The UI follows ArtCraft's design system — the colours, type and metrics are the ones
      <a href="https://github.com/storytold/artcraft" target="_blank" rel="noreferrer noopener">storytold/artcraft</a>
      documents in <code>artcraft-native/src/theme.rs</code>. CorridorKey's own interface is left alone.</p>

      <p><b>Licence.</b> CorridorKey Licence v2.0 — CC BY-NC-SA 4.0 plus additional terms,
      © Corridor Digital LLC, unchanged from upstream. Keying your own shots on a box you rent is
      permitted, including for paid work, and what you produce is yours (§1). <b>Selling access to this
      as an inference service is not</b> (§2b), derivatives must stay free to the public (§2c), and
      organisations over $20M revenue need a purchased seat (§3). The foundation models carry their own
      licences, which travel with them at <code>THIRD_PARTY_NOTICES.md</code>. Read the licence; that is a
      plain reading, not legal advice.</p>

      <table class="kv">
        <tr><td class="hud-label">ck-web</td><td><code>${esc(h.version || "—")}</code></td></tr>
        <tr><td class="hud-label">mode</td><td><code>${esc(h.mode || "—")}</code></td></tr>
        <tr><td class="hud-label">provider</td><td><code>${esc(h.provider || "—")}</code> — ${esc(h.providerWhy || "")}</td></tr>
        <tr><td class="hud-label">checkout</td><td><code>${esc(h.root || "—")}</code></td></tr>
        <tr><td class="hud-label">gpu</td><td>${esc(gpu)}</td></tr>
        <tr><td class="hud-label">weights</td><td>${have} of ${weights.length} present</td></tr>
        <tr><td class="hud-label">fetcher</td><td><code>${esc(h.fetcher || "—")}</code></td></tr>
      </table>

      <p class="hud-label">ONE GPU JOB AT A TIME · DOWNLOADS RUN IN THEIR OWN LANE</p>
    </div>`);
}

// ── plumbing ─────────────────────────────────────────────────────────────────

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function fmtBytes(n) {
  if (!n) return "—";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
}

function hostOf(u) {
  try { return new URL(u).host.replace(/^www\./, ""); } catch { return u; }
}

// The hash is page:clip:view, so a reload — or a link pasted to someone on the
// same box — comes back to the same frame of the same clip rather than to the
// clip list.
function go(page) {
  if (page !== "viewer") {
    // Leaving the viewer: nothing on another page needs frames, so the player
    // stops and every warm fetch is dropped mid-flight. Whatever is already
    // in the cache stays there.
    stopPlay();
    stopWarming();
  }
  state.page = page;
  location.hash = page === "viewer" && state.selected
    ? `viewer:${encodeURIComponent(state.selected)}:${state.view}`
    : page;
  if (page === "viewer") loadFrames().then(render);
  else render();
}

function readHash() {
  const [page, clip, view] = location.hash.slice(1).split(":");
  if (!TITLES[page]) return null;
  return {
    page,
    clip: clip ? decodeURIComponent(clip) : null,
    view: VIEWS.some(([k]) => k === view) ? view : null,
  };
}

async function refresh() {
  try {
    await Promise.all([loadClips(), loadJobs(), loadModules(), loadHealth()]);
    if (state.page === "viewer") await loadFrames();
    if (state.job != null) await loadLog();
    render();
  } catch (e) {
    toast(String(e.message || e), true);
  }
}

// The event stream carries log lines as they are printed, so the console
// does not have to poll.
function listen() {
  const es = new EventSource(withToken("/api/events"));
  es.onmessage = (ev) => {
    const e = JSON.parse(ev.data);
    if (e.kind === "log") {
      if (e.jobId !== state.job) return;
      state.log.push(e.line);
      if (state.log.length > 2000) state.log = state.log.slice(-1200);
      const con = document.getElementById("console");
      if (con) {
        const atBottom = con.scrollHeight - con.scrollTop - con.clientHeight < 40;
        con.insertAdjacentHTML("beforeend", "\n" + logLine(e.line));
        if (atBottom) con.scrollTop = con.scrollHeight;
      }
      return;
    }
    if (e.kind === "job") {
      loadJobs().then(() => {
        if (state.page === "queue") render();
      });
      return;
    }
    if (e.kind === "progress") {
      applyProgress(e.jobId, e.prog);
      return;
    }
    if (e.kind === "modules") {
      Promise.all([loadModules(), loadHealth()]).then(() => {
        if (state.page === "modules") render();
      });
      return;
    }
    if (e.kind === "clips") refresh();
  };
  es.onerror = () => { es.close(); setTimeout(listen, 3000); };
}

document.querySelectorAll(".sidebar .row").forEach((r) =>
  r.addEventListener("click", () => go(r.dataset.page)));
$("#refresh").addEventListener("click", refresh);
$("#toggle-sidebar").addEventListener("click", () =>
  $("#app").classList.toggle("sidebar-closed"));
// A background tab is not being watched, so it has no business rendering
// frames. Both the player and the warmer stop until it comes back.
document.addEventListener("visibilitychange", () => {
  if (document.hidden) {
    stopPlay();
    stopWarming();
  } else if (state.page === "viewer") {
    scheduleWarm(200);
  }
});

// Space plays, the arrows step. Skipped while something is being typed into,
// so the token gate and the clip-name field keep their keys.
window.addEventListener("keydown", (e) => {
  if (state.page !== "viewer" || e.metaKey || e.ctrlKey || e.altKey) return;
  const tag = (e.target.tagName || "").toLowerCase();
  if (tag === "input" || tag === "textarea" || tag === "select") return;
  if (e.key === " ") {
    e.preventDefault();
    playToggle();
  } else if (e.key === "ArrowLeft") {
    e.preventDefault();
    stepBy(-1);
  } else if (e.key === "ArrowRight") {
    e.preventDefault();
    stepBy(1);
  }
});

window.addEventListener("hashchange", () => {
  const h = readHash();
  if (!h) return;
  if (h.clip) state.selected = h.clip;
  if (h.view) state.view = h.view;
  if (h.page !== state.page) go(h.page);
});

wireDock();
const opening = readHash();
if (opening) {
  state.page = opening.page;
  if (opening.clip) state.selected = opening.clip;
  if (opening.view) state.view = opening.view;
}
refresh().then(listen);
setInterval(loadHealth, 10000); // the GPU readout, which nothing pushes
