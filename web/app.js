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
  view: "comp",     // viewer tab
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
  $("#p-despill").addEventListener("input", (e) => ($("#p-despill-v").textContent = e.target.value));
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

const STAGES = {
  "needs-alpha": ["badge-warn", "needs alpha"],
  ready: ["badge-accent", "ready to key"],
  keyed: ["badge-good", "keyed"],
  empty: ["badge-bad", "no input"],
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
          ? `<img loading="lazy" src="${withToken(`/api/clips/${encodeURIComponent(c.name)}/frame?view=${thumbView}&i=0`)}" alt="">`
          : `<span class="empty hud-label">no preview</span>`}
        <div class="corner"><span class="badge ${cls}">${label}</span></div>
        ${c.running ? '<div class="spin"></div>' : ""}
      </div>
      <div class="meta">
        <span class="name">${esc(c.name)}</span>
        <span class="line">
          <span class="hud-label">${esc(c.input || "—")}</span>
          ${c.alpha > 0 ? `<span class="hud-label">· ${c.alpha} hint</span>` : ""}
          ${c.frames > 0 ? `<span class="hud-label">· ${c.frames} keyed</span>` : ""}
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
        state.view = c.stage === "keyed" ? "comp" : c.alpha > 0 ? "alpha" : "input";
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

const VIEWS = [
  ["input", "Plate"],
  ["alpha", "Alpha hint"],
  ["comp", "Comp"],
  ["matte", "Matte"],
  ["fg", "FG"],
  ["processed", "Processed"],
];

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
      await loadFrames();
      render();
    });
    bar.append(b);
  }
  const sp = document.createElement("div");
  sp.style.flex = "1";
  bar.append(sp);
  bar.insertAdjacentHTML("beforeend", `
    <a class="btn btn-mono" href="${withToken(`/api/clips/${encodeURIComponent(state.selected)}/export?view=${state.view}&format=mov`)}">Export ProRes 4444</a>
    <a class="btn btn-mono" href="${withToken(`/api/clips/${encodeURIComponent(state.selected)}/export?view=${state.view}&format=mp4`)}">Export H.264</a>`);
  wrap.append(bar);

  const stage = document.createElement("div");
  stage.className = "stage";
  if (state.view === "input" && clip.input && !clip.input.endsWith("/")) {
    stage.innerHTML = `<video controls preload="metadata" src="${withToken(`/api/clips/${encodeURIComponent(state.selected)}/input`)}"></video>`;
  } else if (state.frames.length) {
    stage.innerHTML = `<img src="${withToken(`/api/clips/${encodeURIComponent(state.selected)}/frame?view=${state.view}&i=${state.frame}`)}" alt="">`;
  } else {
    stage.innerHTML = `<div class="empty">Nothing in <b>${state.view}</b> yet.<br><span class="hud-label">run the step that writes it</span></div>`;
  }
  wrap.append(stage);

  if (state.frames.length > 1) {
    const scrub = document.createElement("div");
    scrub.className = "scrub";
    scrub.innerHTML = `
      <span class="hud-label">Frame</span>
      <input type="range" min="0" max="${state.frames.length - 1}" value="${state.frame}">
      <span class="frame-no">${state.frame + 1} / ${state.frames.length}</span>`;
    const range = scrub.querySelector("input");
    range.addEventListener("input", () => {
      state.frame = Number(range.value);
      scrub.querySelector(".frame-no").textContent = `${state.frame + 1} / ${state.frames.length}`;
      const img = stage.querySelector("img");
      if (img) img.src = withToken(`/api/clips/${encodeURIComponent(state.selected)}/frame?view=${state.view}&i=${state.frame}`);
    });
    wrap.append(scrub);
  }
  body.append(wrap);
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
  body.insertAdjacentHTML("beforeend", `
    <div class="hint" style="display:flex;flex-direction:column;gap:12px">
      <p><b>ck-web</b> is a browser front end for
      <a href="https://github.com/nikopueringer/CorridorKey" target="_blank" rel="noreferrer noopener">CorridorKey</a>
      as packaged by <a href="https://github.com/EvanRuiz/EZ-CorridorKey" target="_blank" rel="noreferrer noopener">EZ-CorridorKey</a>.
      It does not reimplement the keyer: every job here runs the project's own
      <code>clip_manager.py</code> and reads back what that writes.</p>
      <p>The pipeline is three steps on disk:</p>
      <ol>
        <li><code>ClipsForInference/&lt;clip&gt;/Input.mov</code> — the plate you upload.</li>
        <li><code>AlphaHint/</code> — a coarse matte, from GVM (<i>Generate alpha hint</i>) or your own
        file named <code>alphaHint.mov</code>.</li>
        <li><code>Output/{Comp,Matte,FG,Processed}</code> — what the keyer writes. Comp is 8-bit PNG,
        the rest are half-float EXR (DWAB), converted to PNG on the fly for this viewer.</li>
      </ol>
      <p>The UI follows ArtCraft's design system — the colours, type and metrics are the ones
      <a href="https://github.com/storytold/artcraft" target="_blank" rel="noreferrer noopener">storytold/artcraft</a>
      documents in <code>artcraft-native/src/theme.rs</code>. CorridorKey's own interface is left alone.</p>
      <p>Keying is done by a <b>provider</b>, and the models it needs are <b>modules</b> fetched at runtime.
      The image carries neither — it is the interface and the setup machinery, nothing else. The
      <i>mock</i> provider keys with a plain green-excess formula in Go, which is enough to exercise every
      screen here without a GPU, a Python install or a single downloaded byte.</p>
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

function go(page) {
  state.page = page;
  location.hash = page;
  if (page === "viewer") loadFrames().then(render);
  else render();
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
window.addEventListener("hashchange", () => {
  const p = location.hash.slice(1);
  if (TITLES[p] && p !== state.page) go(p);
});

wireDock();
if (TITLES[location.hash.slice(1)]) state.page = location.hash.slice(1);
refresh().then(listen);
setInterval(loadHealth, 10000); // the GPU readout, which nothing pushes
