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
			if queue.isBootstrapping() {
				return false, "the inference stack is still downloading into " + venvDir()
			}
			return false, "no interpreter at " + cfg.Python + " (the venv is what has torch)"
		}
	}
	for _, m := range modules {
		if !m.Required {
			continue
		}
		if !m.state().Present {
			if queue.isInstalling(m.Key) {
				return false, m.Title + " is still downloading"
			}
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

// ck2Views maps infer.py's output onto what the viewer reads. There is one
// entry because there is one output: runtime.py appends _alpha_exr to the
// clip name, and that folder is the matte.
var ck2Views = []struct{ suffix, stage string }{
	{"_alpha_exr", "Matte"},
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

	// Scratch of its own, under the clip, so the prepasses can be watched and
	// so a killed run does not leave several GB in /tmp. The runtime removes
	// its own memmaps on a clean exit; this clears what a kill left behind.
	tmp := ck2TempDir(clip)
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o775); err != nil {
		return err
	}

	input, plan, err := ck2Fit(ctx, dir, input, p, log)
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
		// The matte, and nothing else. fg, cutout and the checkerboard comp are
		// three more EXR sequences and more GPU time for pictures this UI does
		// not deliver — it renders a comp from the plate and the matte instead.
		"--outputs", "alpha",
		// EXR keeps the matte and the foreground unclamped and scene-linear.
		// The preview encode would throw that away, which is the whole reason
		// to run this rather than the drag-and-drop launcher.
		"--output_format", "exr",
		"--exr_dtype", "half",
		"--ffmpeg_bin", cfg.FFmpeg,
		"--temp_dir", tmp,
	}
	// An explicit window turns the runtime's own sizing off, which is the
	// point: ck2Fit already made that choice against a wider margin.
	if plan.Chunk > 0 {
		args = append(args, "--hann_chunk", strconv.Itoa(plan.Chunk),
			"--hann_stride", strconv.Itoa(plan.Stride))
		if plan.LowVram {
			args = append(args, "--low_vram")
		}
	}
	if p.HintQuality > 0 {
		args = append(args, "--hint_quality", strconv.FormatFloat(p.HintQuality, 'f', 2, 64))
	}
	if p.LowVram && !plan.LowVram {
		// The toggle only ever adds staging; it cannot take it away, because
		// the plan asked for it to fit the card at all.
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
	_ = os.RemoveAll(tmp)
	return ck2Collect(stage, dir, log)
}

// ck2Collect moves infer.py's output folder to Output/Matte. It is written as
// a loop over ck2Views because the mapping, not the count, is the point: if a
// later runtime grows another output worth keeping, it is one line here.
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
			if name := e.Name(); strings.HasSuffix(name, v.suffix) {
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

// ck2Fit decides the Hann window this clip runs with, and resizes the plate
// when not even a single-frame window fits the card. It returns the plate to
// key, which is the original unless it had to be shrunk.
func ck2Fit(ctx context.Context, dir, input string, p Params, log func(string)) (string, VRAMPlan, error) {
	budget := gpuFreeGB()
	if budget <= 0 {
		log("could not read free VRAM from nvidia-smi — leaving the window to the runtime")
		return input, VRAMPlan{}, nil
	}
	info, err := probePlate(input)
	if err != nil {
		log(err.Error() + " — leaving the window to the runtime")
		return input, VRAMPlan{}, nil
	}
	w, h, frames := info.W, info.H, info.Frames

	plan := planVRAM(w, h, frames, budget)
	log("VRAM: " + plan.Summary())
	if plan.Fits {
		// A short window still keys the clip; it just holds the matte together
		// over fewer frames, so it is worth knowing what a longer one costs.
		if bw, bh := resolutionForWindow(w, h, frames, budget, 24); bw > 0 {
			log(fmt.Sprintf("VRAM: a 24-frame window — steadier edges over time — "+
				"would need the plate at %dx%d on this card", bw, bh))
		}
		return input, plan, nil
	}
	if plan.FitW == 0 {
		return input, plan, fmt.Errorf("%dx%d will not fit in %.1f GB at any resolution", w, h, budget)
	}
	if !p.FitVRAM {
		return input, plan, fmt.Errorf("this plate needs more VRAM than the card has free. "+
			"Resize it to %dx%d and key that, or turn Fit to VRAM on and ck-web will resize it for you",
			plan.FitW, plan.FitH)
	}

	small, err := ck2Downscale(ctx, dir, input, plan.FitW, plan.FitH, log)
	if err != nil {
		return input, plan, err
	}
	fitted := planVRAM(plan.FitW, plan.FitH, frames, budget)
	log("VRAM: " + fitted.Summary())
	if !fitted.Fits {
		return input, plan, fmt.Errorf("even %dx%d does not fit — the card lost memory to something else mid-plan",
			plan.FitW, plan.FitH)
	}
	return small, fitted, nil
}

// ck2Downscale writes a smaller copy of the plate beside it, once, and keeps
// it: the same clip keyed again on the same card wants the same size. Lanczos
// and 4:4:4 because this is the plate a matte is pulled from, not a preview —
// chroma subsampling would be thrown away exactly at the green edge that
// matters. The output is smaller than the plate, so the matte is too: that is
// the trade this is making, and the log says so out loud.
func ck2Downscale(ctx context.Context, dir, input string, w, h int, log func(string)) (string, error) {
	out := filepath.Join(dir, ".fit", fmt.Sprintf("%dx%d.mp4", w, h))
	if st, err := os.Stat(out); err == nil && st.Size() > 0 {
		if src, err := os.Stat(input); err == nil && st.ModTime().After(src.ModTime()) {
			log(fmt.Sprintf("reusing the %dx%d copy made earlier", w, h))
			return out, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o775); err != nil {
		return "", err
	}
	log(fmt.Sprintf("not enough VRAM for the native plate — resizing it to %dx%d first. "+
		"The matte comes back at %dx%d, so scale it up in comp, or key this clip on a bigger card.",
		w, h, w, h))
	cmd := exec.CommandContext(ctx, cfg.FFmpeg, "-v", "error", "-y", "-i", input,
		"-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", w, h),
		"-c:v", "libx264", "-crf", "10", "-preset", "medium", "-pix_fmt", "yuv444p",
		"-an", out)
	if err := runLogged(cmd, log); err != nil {
		_ = os.Remove(out)
		return "", fmt.Errorf("could not resize the plate: %w", err)
	}
	log("resized plate written to " + out)
	return out, nil
}
