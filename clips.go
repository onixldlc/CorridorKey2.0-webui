package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Stage is how far a clip has travelled through the pipeline. It mirrors what
// clip_manager.py looks for on disk: a video (or Input/ sequence), an
// AlphaHint/ folder, then Output/.
type Stage string

const (
	StageNeedsAlpha Stage = "needs-alpha" // input only
	StageReady      Stage = "ready"       // input + alpha hint, not keyed
	StageKeyed      Stage = "keyed"       // Output/Matte has frames
	StageEmpty      Stage = "empty"       // folder with no usable input
)

// Clip is one folder under ClipsForInference.
type Clip struct {
	Name    string `json:"name"`
	Stage   Stage  `json:"stage"`
	Input   string `json:"input"`  // basename of the video, or "Input/" for a sequence
	Frames  int    `json:"frames"` // Comp frames written so far
	Alpha   int    `json:"alpha"`  // AlphaHint frames
	Size    int64  `json:"size"`   // input bytes
	Preview string `json:"preview"`
	Running bool   `json:"running"`
}

var videoExt = map[string]bool{
	".mov": true, ".mp4": true, ".mkv": true, ".avi": true, ".mxf": true,
	".webm": true, ".m4v": true, ".r3d": true, ".braw": true, ".prores": true,
}

var imageExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".tif": true, ".tiff": true, ".exr": true,
}

// listClips scans the uploads folder. Folders the CLI itself skips
// (IgnoredClips, Output) are skipped here too.
func listClips() ([]Clip, error) {
	entries, err := os.ReadDir(cfg.ClipsDir)
	if err != nil {
		return nil, err
	}
	var out []Clip
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.Name() == "IgnoredClips" || e.Name() == "Output" {
			continue
		}
		out = append(out, inspectClip(e.Name()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func inspectClip(name string) Clip {
	dir := filepath.Join(cfg.ClipsDir, name)
	c := Clip{Name: name, Stage: StageEmpty}

	if st, err := os.Stat(filepath.Join(dir, "Input")); err == nil && st.IsDir() {
		if n := countFiles(filepath.Join(dir, "Input"), imageExt); n > 0 {
			c.Input = "Input/"
			c.Stage = StageNeedsAlpha
		}
	}
	if c.Input == "" {
		if f, size := firstVideo(dir); f != "" {
			c.Input, c.Size = f, size
			c.Stage = StageNeedsAlpha
		}
	}
	if c.Input == "" {
		return c
	}

	c.Alpha = countFiles(filepath.Join(dir, "AlphaHint"), imageExt)
	if c.Alpha == 0 {
		c.Alpha = countFiles(filepath.Join(dir, "alphahint"), imageExt)
	}
	if c.Alpha == 0 && hasAlphaVideo(dir) {
		c.Alpha = -1 // present as a video; frame count unknown without a probe
	}
	if c.Alpha != 0 {
		c.Stage = StageReady
	}

	// The matte is the deliverable, so it is also what "keyed" means now.
	c.Frames = countFiles(filepath.Join(dir, "Output", "Matte"), imageExt)
	if c.Frames > 0 {
		c.Stage = StageKeyed
		c.Preview = "matte"
	} else if c.Alpha > 0 {
		c.Preview = "hint"
	}
	c.Running = queue.isRunning(name)
	return c
}

func firstVideo(dir string) (string, int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0
	}
	// Input.* wins, as in clip_manager.py; otherwise the first video found.
	var fallback string
	var fallbackSize int64
	for _, e := range entries {
		if e.IsDir() || !videoExt[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		stem := strings.ToLower(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		if stem == "input" {
			return e.Name(), size
		}
		if strings.HasPrefix(stem, "alphahint") {
			continue
		}
		if fallback == "" {
			fallback, fallbackSize = e.Name(), size
		}
	}
	return fallback, fallbackSize
}

func hasAlphaVideo(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		stem := strings.ToLower(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		if strings.HasPrefix(stem, "alphahint") && videoExt[strings.ToLower(filepath.Ext(e.Name()))] {
			return true
		}
	}
	return false
}

func countFiles(dir string, exts map[string]bool) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && exts[strings.ToLower(filepath.Ext(e.Name()))] {
			n++
		}
	}
	return n
}

// frameList returns the sorted PNG/EXR frames of one output view.
func frameList(clip, view string) ([]string, string) {
	dir := viewDir(clip, view)
	if dir == "" {
		return nil, ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, dir
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && imageExt[strings.ToLower(filepath.Ext(e.Name()))] {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, dir
}

// viewDir maps a UI view name to a folder of frames. There are three of them
// now — the plate, the hint that went in, the matte that came out — because
// those are the only three things this UI deals in. "comp" is not here: it is
// rendered on request from the plate and the matte, and nothing writes it.
//
// The old names are still accepted so a page left open on an older build does
// not start asking for folders that no longer mean anything.
func viewDir(clip, view string) string {
	base := filepath.Join(cfg.ClipsDir, clip)
	switch view {
	case "matte":
		return filepath.Join(base, "Output", "Matte")
	case "hint", "alpha":
		if st, err := os.Stat(filepath.Join(base, "AlphaHint")); err == nil && st.IsDir() {
			return filepath.Join(base, "AlphaHint")
		}
		return filepath.Join(base, "alphahint")
	case "plate", "input":
		return filepath.Join(base, "Input")
	}
	return ""
}

// safeName keeps an uploaded clip name to one path segment.
func safeName(s string) string {
	s = filepath.Base(strings.TrimSpace(s))
	s = strings.NewReplacer("/", "_", "\\", "_", "..", "_", ":", "_").Replace(s)
	if s == "" || s == "." {
		return ""
	}
	return s
}
