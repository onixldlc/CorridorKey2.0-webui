package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, providerInfo())
}

// handleProvision reports the self-provisioning decision, and on POST makes
// it again. The retry is the point: a box that was short of disk, or had not
// brought up its GPU yet, becomes a production box without a restart.
func handleProvision(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, provisionStatus())
	case http.MethodPost:
		p := autoProvision()
		logProvision(p)
		writeJSON(w, p)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func handleModules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, moduleStates())
}

// handleModuleRoutes serves /api/modules/{key}[/install].
func handleModuleRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/modules/")
	parts := strings.Split(rest, "/")
	m := moduleByKey(parts[0])
	if m == nil {
		http.Error(w, "no such module", http.StatusNotFound)
		return
	}
	verb := ""
	if len(parts) > 1 {
		verb = parts[1]
	}
	switch {
	case verb == "" && r.Method == http.MethodGet:
		writeJSON(w, m.state())
	case verb == "install" && r.Method == http.MethodPost:
		if cfg.Dev {
			http.Error(w, "this is a dev run (-mode dev): downloads are off, and the mock provider needs none",
				http.StatusForbidden)
			return
		}
		if queue.isInstalling(m.Key) {
			http.Error(w, "already installing", http.StatusConflict)
			return
		}
		writeJSON(w, queue.SubmitInstall(m.Key))
	case verb == "" && r.Method == http.MethodDelete:
		if err := uninstall(m.Key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		broadcast(event{Kind: "modules"})
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "unknown route", http.StatusNotFound)
	}
}

func handleClips(w http.ResponseWriter, r *http.Request) {
	clips, err := listClips()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if clips == nil {
		clips = []Clip{}
	}
	writeJSON(w, clips)
}

// handleClipRoutes serves /api/clips/{name}/{verb}[/{arg}].
func handleClipRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/clips/")
	parts := strings.Split(rest, "/")
	name := safeName(parts[0])
	if name == "" {
		http.Error(w, "no clip name", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(cfg.ClipsDir, name)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		http.Error(w, "no such clip", http.StatusNotFound)
		return
	}
	verb := ""
	if len(parts) > 1 {
		verb = parts[1]
	}

	switch {
	case verb == "" && r.Method == http.MethodGet:
		writeJSON(w, inspectClip(name))
	case verb == "" && r.Method == http.MethodDelete:
		if queue.isRunning(name) {
			http.Error(w, "clip is in a running job", http.StatusConflict)
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		broadcast(event{Kind: "clips"})
		writeJSON(w, map[string]any{"ok": true})
	case verb == "alpha" && r.Method == http.MethodPost:
		j := queue.Submit("generate_alphas", name, defaultParams())
		writeJSON(w, j)
	case verb == "key" && r.Method == http.MethodPost:
		p := defaultParams()
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&p)
		}
		j := queue.Submit("run_inference", name, p)
		writeJSON(w, j)
	case verb == "frames" && r.Method == http.MethodGet:
		view := q(r, "view", "comp")
		names, _ := frameList(name, view)
		if names == nil {
			names = []string{}
		}
		writeJSON(w, map[string]any{"view": view, "count": len(names), "frames": names})
	case verb == "frame" && r.Method == http.MethodGet:
		serveFrame(w, r, name)
	case verb == "input" && r.Method == http.MethodGet:
		c := inspectClip(name)
		if c.Input == "" || strings.HasSuffix(c.Input, "/") {
			http.Error(w, "no input video to stream", http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, c.Input))
	case verb == "export" && r.Method == http.MethodGet:
		serveExport(w, r, name)
	default:
		http.Error(w, "unknown route", http.StatusNotFound)
	}
}

// serveFrame sends one frame of one view. PNGs go out as they are; EXRs are
// converted on the fly, because browsers cannot show them.
func serveFrame(w http.ResponseWriter, r *http.Request, clip string) {
	view := q(r, "view", "comp")
	names, dir := frameList(clip, view)
	if len(names) == 0 {
		http.Error(w, "no frames in "+view, http.StatusNotFound)
		return
	}
	idx, _ := strconv.Atoi(q(r, "i", "0"))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(names) {
		idx = len(names) - 1
	}
	path := filepath.Join(dir, names[idx])

	if strings.EqualFold(filepath.Ext(path), ".exr") {
		w.Header().Set("Content-Type", "image/png")
		// Linear EXR to sRGB PNG; a matte is single-channel, so greyscale it.
		cmd := exec.Command(cfg.FFmpeg, "-v", "error", "-i", path,
			"-vf", "scale=in_color_matrix=bt709:out_color_matrix=bt709,format=rgb24",
			"-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "-")
		out, err := cmd.Output()
		if err != nil {
			http.Error(w, "exr conversion failed (is ffmpeg built with OpenEXR?): "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(out)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

// serveExport stitches a view's frames into a download. ProRes 4444 carries
// the alpha; h264 is for a quick look.
func serveExport(w http.ResponseWriter, r *http.Request, clip string) {
	view := q(r, "view", "comp")
	format := q(r, "format", "mp4")
	names, dir := frameList(clip, view)
	if len(names) == 0 {
		http.Error(w, "no frames in "+view, http.StatusNotFound)
		return
	}
	fps := q(r, "fps", "24")

	var args []string
	var ctype, ext string
	switch format {
	case "mov":
		ctype, ext = "video/quicktime", "mov"
		// Fragmented, because the mov muxer cannot seek back to write a moov
		// atom when its output is a pipe: "muxer does not support non
		// seekable output", and the response body is a pipe.
		args = []string{"-c:v", "prores_ks", "-profile:v", "4444", "-pix_fmt", "yuva444p10le",
			"-movflags", "frag_keyframe+empty_moov", "-f", "mov"}
	case "mp4":
		ctype, ext = "video/mp4", "mp4"
		args = []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "18",
			"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", "-movflags", "frag_keyframe+empty_moov", "-f", "mp4"}
	default:
		http.Error(w, "format must be mp4 or mov", http.StatusBadRequest)
		return
	}

	cmd := exec.Command(cfg.FFmpeg, append([]string{
		"-v", "error", "-framerate", fps,
		"-pattern_type", "glob", "-i", filepath.Join(dir, "*"+filepath.Ext(names[0])),
		"-r", fps,
	}, append(args, "-")...)...)

	// The response header is held back until ffmpeg actually produces a byte,
	// so an export that fails outright answers with an error instead of a
	// 200 carrying an empty file.
	out := &deferredWriter{w: w, header: func() {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", "attachment; filename=\""+clip+"_"+view+"."+ext+"\"")
	}}
	var errBuf bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)

	if err := cmd.Run(); err != nil {
		if out.wrote == 0 {
			http.Error(w, "ffmpeg could not write that export: "+firstLine(errBuf.String()), http.StatusInternalServerError)
			return
		}
		// Bytes are already on the wire; the short file is the only signal left.
		os.Stderr.WriteString("ck-web: export failed mid-stream: " + err.Error() + "\n")
	}
}

// deferredWriter sets the response header on the first byte written.
type deferredWriter struct {
	w      http.ResponseWriter
	header func()
	wrote  int64
}

func (d *deferredWriter) Write(p []byte) (int, error) {
	if d.wrote == 0 && len(p) > 0 {
		d.header()
	}
	n, err := d.w.Write(p)
	d.wrote += int64(n)
	if f, ok := d.w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

// firstLine trims ffmpeg's diagnostics down to something a toast can hold.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "no output and no diagnostics"
	}
	return s
}

// handleUpload takes one video and makes a clip folder for it, named after
// the file unless ?name= says otherwise.
func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var created []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		ext := strings.ToLower(filepath.Ext(part.FileName()))
		role := "Input"
		if strings.Contains(strings.ToLower(part.FileName()), "alphahint") {
			role = "AlphaHint"
		}
		if !videoExt[ext] {
			part.Close()
			http.Error(w, "unsupported file type "+ext, http.StatusBadRequest)
			return
		}
		name := safeName(strings.TrimSuffix(filepath.Base(part.FileName()), filepath.Ext(part.FileName())))
		if v := safeName(r.URL.Query().Get("name")); v != "" {
			name = v
		}
		if name == "" {
			part.Close()
			http.Error(w, "could not derive a clip name", http.StatusBadRequest)
			return
		}
		dir := filepath.Join(cfg.ClipsDir, name)
		if err := os.MkdirAll(dir, 0o775); err != nil {
			part.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dst := filepath.Join(dir, role+ext)
		f, err := os.Create(dst)
		if err != nil {
			part.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, copyErr := io.Copy(f, part)
		closeErr := f.Close()
		part.Close()
		if copyErr != nil || closeErr != nil {
			os.Remove(dst)
			http.Error(w, "upload failed mid-write", http.StatusInternalServerError)
			return
		}
		created = append(created, name)
	}

	if len(created) == 0 {
		http.Error(w, "no file part in the request", http.StatusBadRequest)
		return
	}
	broadcast(event{Kind: "clips"})
	writeJSON(w, map[string]any{"ok": true, "clips": created})
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Action string `json:"action"`
			Clip   string `json:"clip"`
			Params Params `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Action != "generate_alphas" && body.Action != "run_inference" {
			http.Error(w, "action must be generate_alphas or run_inference", http.StatusBadRequest)
			return
		}
		if body.Params.Despill == 0 && body.Params.SpeckSize == 0 && body.Params.Refiner == 0 {
			body.Params = defaultParams()
		}
		writeJSON(w, queue.Submit(body.Action, safeName(body.Clip), body.Params))
		return
	}
	writeJSON(w, queue.List())
}

// handleJobRoutes serves /api/jobs/{id}[/log|/cancel].
func handleJobRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	parts := strings.Split(rest, "/")
	id, err := strconv.Atoi(parts[0])
	if err != nil {
		http.Error(w, "bad job id", http.StatusBadRequest)
		return
	}
	j := queue.Get(id)
	if j == nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	verb := ""
	if len(parts) > 1 {
		verb = parts[1]
	}
	switch verb {
	case "":
		writeJSON(w, j)
	case "log":
		n, _ := strconv.Atoi(q(r, "tail", "500"))
		writeJSON(w, map[string]any{"id": j.ID, "state": j.State, "lines": j.Tail(n)})
	case "cancel":
		writeJSON(w, map[string]any{"cancelled": queue.Cancel(id)})
	default:
		http.Error(w, "unknown route", http.StatusNotFound)
	}
}

func q(r *http.Request, key, def string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	return def
}
