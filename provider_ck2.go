package main

// The keyer this UI drives is upstream's own: infer.py at cfg.Root, which the
// server shells out to. Nothing is reimplemented here. master carries no
// Python at all — the image clones the runtime from the upstream branch into
// /opt/corridorkey, and that is what cfg.Root points at there.
//
// 2.0 is a far better fit for a web front end than the 1.x line was. infer.py
// takes --input and --output_dir, so one clip is keyed without disturbing the
// others, and every parameter is a real flag rather than an answer typed at an
// input() prompt. A hint is an optional input to the key itself, so there is
// no separate hint pass to sequence.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type CorridorKey2Provider struct{}

func (CorridorKey2Provider) ID() string    { return "corridorkey2" }
func (CorridorKey2Provider) Title() string { return "CorridorKey 2.0 (infer.py)" }

func (CorridorKey2Provider) NeedsModules() []string {
	return []string{"corridorkey2", "cradio", "moge", "rvm"}
}

func (CorridorKey2Provider) Ready() (bool, string) {
	if _, err := os.Stat(filepath.Join(cfg.Root, "infer.py")); err != nil {
		return false, "no infer.py under " + cfg.Root + " (-root must point at a CorridorKey 2.0 checkout)"
	}
	if _, err := exec.LookPath(cfg.Python); err != nil {
		if _, err := os.Stat(cfg.Python); err != nil {
			return false, "no interpreter at " + cfg.Python + " (the venv is what has torch)"
		}
	}
	for _, m := range modules {
		if !m.Required {
			continue
		}
		if !m.state().Present {
			return false, m.Title + " is not downloaded yet — install it on the Modules page"
		}
	}
	// infer.py refuses a CPU-only box on the first line of main(), so say so
	// here instead of letting the first job die with a traceback.
	if len(gpuInfo()) == 0 {
		return false, "CorridorKey 2.0 needs a CUDA GPU (RTX 30-series or newer) and nvidia-smi reported none"
	}
	return true, "infer.py, the weights and a GPU are all present"
}

// ck2Views maps infer.py's outputs onto the four stages the viewer knows.
// runtime.py appends these to the clip name; checker carries the despill
// strength in its own suffix, so that one is matched on the stem.
var ck2Views = []struct{ suffix, stage string }{
	{"_alpha_exr", "Matte"},
	{"_fg_exr", "FG"},
	{"_cutout_exr", "Processed"},
	{"_checker_comp_despill", "Comp"}, // the number and _exr follow
}

func (CorridorKey2Provider) Run(ctx context.Context, action, clip string, p Params, log func(string)) error {
	if clip == "" {
		return fmt.Errorf("this provider keys one clip at a time; no clip was given")
	}
	if action == "generate_alphas" {
		return fmt.Errorf("CorridorKey 2.0 needs no alpha hint pass — key the clip directly. " +
			"If you upload one, AlphaHint/ is handed to the model as --hint_video")
	}

	dir := filepath.Join(cfg.ClipsDir, clip)
	input, err := findInput(dir)
	if err != nil {
		return err
	}

	// infer.py names its outputs after the input file, so it gets a staging
	// folder and the results are sorted into Output/ once it exits clean.
	stage := filepath.Join(dir, "Output", ".v2")
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o775); err != nil {
		return err
	}

	args := []string{
		"infer.py",
		"--input", input,
		"--output_dir", stage,
		// All four, so every stage the viewer offers has frames behind it.
		"--outputs", "alpha", "fg", "checker", "cutout",
		// EXR keeps the matte and the foreground unclamped and scene-linear.
		// The preview encode would throw that away, which is the whole reason
		// to run this rather than the drag-and-drop launcher.
		"--output_format", "exr",
		"--exr_dtype", "half",
		"--despill_strength", strconv.FormatFloat(p.despillStrength(), 'f', 2, 64),
		"--ffmpeg_bin", cfg.FFmpeg,
	}
	if p.HintQuality > 0 {
		args = append(args, "--hint_quality", strconv.FormatFloat(p.HintQuality, 'f', 2, 64))
	}
	if p.LowVram {
		args = append(args, "--low_vram")
	}
	if p.Linear {
		args = append(args, "--frame_dir_linear")
	}
	if hint := filepath.Join(dir, "AlphaHint"); countFiles(hint, imageExt) > 0 {
		args = append(args, "--hint_video", hint)
		log("AlphaHint/ found — passing it as --hint_video")
	}

	cmd := exec.CommandContext(ctx, cfg.Python, args...)
	cmd.Dir = cfg.Root
	cmd.Env = append(os.Environ(),
		"PYTHONUNBUFFERED=1",
		"OPENCV_IO_ENABLE_OPENEXR=1",
		"HF_HUB_DISABLE_PROGRESS_BARS=1",
	)
	log("$ " + cfg.Python + " " + strings.Join(args, " "))
	if err := runLogged(cmd, log); err != nil {
		return err
	}
	return ck2Collect(stage, dir, log)
}

// ck2Collect sorts infer.py's output folders into Output/<stage>/. Comp is
// transcoded to PNG on the way, because that is what the clip scanner counts
// and what the grid tiles load.
func ck2Collect(stage, dir string, log func(string)) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	moved := 0
	for _, v := range ck2Views {
		var src string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if v.suffix == "_checker_comp_despill" {
				if strings.Contains(name, v.suffix) && strings.HasSuffix(name, "_exr") {
					src = filepath.Join(stage, name)
				}
				continue
			}
			if strings.HasSuffix(name, v.suffix) {
				src = filepath.Join(stage, name)
			}
		}
		if src == "" {
			log("no " + v.suffix + " output — skipping " + v.stage)
			continue
		}
		dst := filepath.Join(dir, "Output", v.stage)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if v.stage == "Comp" {
			if err := ck2CompToPNG(src, dst, log); err != nil {
				return err
			}
			moved++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o775); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("could not move %s into %s: %w", filepath.Base(src), v.stage, err)
		}
		log(v.stage + " ← " + filepath.Base(src))
		moved++
	}
	if moved == 0 {
		return fmt.Errorf("infer.py wrote nothing recognisable into %s", stage)
	}
	_ = os.RemoveAll(stage) // staging only; anything left is debris
	return nil
}

// ck2CompToPNG renders the checkerboard comp to 8-bit PNG with the same
// transfer the viewer applies to any other scene-linear EXR.
func ck2CompToPNG(src, dst string, log func(string)) error {
	if err := os.MkdirAll(dst, 0o775); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".exr") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("no EXR frames in %s", src)
	}
	for i, n := range names {
		out := filepath.Join(dst, fmt.Sprintf("%05d.png", i))
		cmd := exec.Command(cfg.FFmpeg, "-v", "error", "-y", "-i", filepath.Join(src, n),
			"-vf", "scale=in_color_matrix=bt709:out_color_matrix=bt709,format=rgb24",
			"-frames:v", "1", out)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("comp frame %s would not convert to png: %w", n, err)
		}
	}
	log(fmt.Sprintf("Comp ← %d frames transcoded to png", len(names)))
	return os.RemoveAll(src)
}

// findInput locates the uploaded plate inside a clip folder.
func findInput(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(strings.ToLower(e.Name()), "input.") {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no Input.* plate in %s", dir)
}
