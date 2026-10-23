package main

// What a clip costs on the card, and what to do when it does not fit.
//
// The runtime already sizes its own Hann window from free VRAM, but its safety
// margin is 1.1x the estimate plus 0.3 GB, and that is too thin: a 3840x2160
// plate on a 24 GB card estimates 20.8 GB, clears the margin by 0.14 GB, runs
// for twenty minutes and then dies on "tried to allocate 2.24 GiB". So ck-web
// makes the choice itself, from the same measured table, with a margin wide
// enough to have rejected that window, and passes --hann_chunk explicitly so
// the runtime's automatic selection never runs.
//
// The table is corridorkey/runtime.py's _VRAM_PEAK_GB, transcribed in the same
// shape so a diff against upstream stays readable. The numbers are measured
// peaks in GiB by megapixels of model input, per Hann chunk size.

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type vramRow struct {
	mp    float64
	peaks []float64
}

var (
	vramChunksNormal = []int{8, 24, 48, 80}
	vramRowsNormal   = []vramRow{
		{0.98, []float64{6.19, 7.52, 9.77, 12.89}},
		{2.21, []float64{7.50, 11.33, 18.66, 24.89}},
		{3.93, []float64{9.06, 14.78, 23.92, 33.19}},
		{8.36, []float64{13.94, 25.96, 45.15, 70.45}},
	}
	vramChunksLow = []int{1, 2, 4, 8, 24, 48, 80}
	vramRowsLow   = []vramRow{
		{0.98, []float64{2.71, 2.78, 2.94, 3.25, 4.54, 6.57, 9.50}},
		{2.21, []float64{2.80, 2.97, 3.35, 4.12, 7.69, 13.20, 20.56}},
		{3.93, []float64{2.92, 3.23, 3.85, 5.09, 10.09, 17.57, 27.75}},
		{8.36, []float64{3.25, 3.92, 5.28, 8.38, 20.78, 39.66, 64.65}},
	}
)

const (
	// Upstream's margin is 1.35 and 0.5 here instead of 1.1 and 0.3: the clip
	// that failed cleared the old margin and then could not find 2.24 GB, so
	// the true peak was at least a fifth above the estimate.
	vramMarginScale = 1.35
	vramMarginGB    = 0.5

	// The runtime pads the model input up to a multiple of this before it
	// measures anything, so 2160 is really 2176 and the megapixel figure the
	// table is indexed by is the padded one.
	shapeQuantum = 128
)

func vramTable(lowVram bool) ([]int, []vramRow) {
	if lowVram {
		return vramChunksLow, vramRowsLow
	}
	return vramChunksNormal, vramRowsNormal
}

// interp is piecewise-linear through (x, y), extrapolating past both ends —
// upstream's _interp, including the way it extrapolates rather than clamping.
func interp(x float64, xs, ys []float64) float64 {
	for i := 0; i+1 < len(xs); i++ {
		if x <= xs[i+1] || i+2 == len(xs) {
			return ys[i] + (ys[i+1]-ys[i])*(x-xs[i])/(xs[i+1]-xs[i])
		}
	}
	return ys[0]
}

// estimatePeakVRAM is the peak for a Hann window of frames frames at this many
// megapixels of model input.
func estimatePeakVRAM(megapixels float64, frames int, lowVram bool) float64 {
	chunks, rows := vramTable(lowVram)
	if frames < chunks[0] {
		frames = chunks[0]
	}
	mps := make([]float64, len(rows))
	for i, r := range rows {
		mps[i] = r.mp
	}
	xs := make([]float64, len(chunks))
	perChunk := make([]float64, len(chunks))
	for i := range chunks {
		col := make([]float64, len(rows))
		for j, r := range rows {
			col[j] = r.peaks[i]
		}
		// The floor is upstream's: extrapolating below the smallest measured
		// resolution must not predict less than that resolution cost.
		perChunk[i] = interp(megapixels, mps, col)
		if perChunk[i] < col[0] {
			perChunk[i] = col[0]
		}
		xs[i] = float64(chunks[i])
	}
	return interp(float64(frames), xs, perChunk)
}

// VRAMPlan is the decision for one clip: the window to run, whether it fits,
// and the resolution to fall back to when it does not.
type VRAMPlan struct {
	SrcW       int     `json:"srcW,omitempty"` // the plate as shot
	SrcH       int     `json:"srcH,omitempty"`
	ModelW     int     `json:"modelW"` // padded to shapeQuantum
	ModelH     int     `json:"modelH"`
	Megapixels float64 `json:"megapixels"`
	Frames     int     `json:"frames"`
	Chunk      int     `json:"chunk"`
	Stride     int     `json:"stride"`
	LowVram    bool    `json:"lowVram"`
	EstGB      float64 `json:"estGB"`
	BudgetGB   float64 `json:"budgetGB"`
	Fits       bool    `json:"fits"`
	FitW       int     `json:"fitW,omitempty"` // a plate size that does fit
	FitH       int     `json:"fitH,omitempty"`
}

func ceilTo(v, q int) int { return (v + q - 1) / q * q }

// planVRAM picks the largest window that fits the budget at this plate size.
// Bigger windows hold the matte together over time, and low-VRAM mode is exact
// rather than approximate, so a larger low-VRAM window beats a smaller normal
// one — the same preference order the runtime uses.
func planVRAM(srcW, srcH, frames int, budgetGB float64) VRAMPlan {
	p := planWindow(srcW, srcH, frames, budgetGB)
	if !p.Fits {
		// Only the outer call looks for a resolution that fits, so the search
		// below cannot recurse into itself.
		p.FitW, p.FitH = fitResolution(srcW, srcH, p.Frames, budgetGB)
	}
	return p
}

// planWindow is planVRAM without the fallback search.
func planWindow(srcW, srcH, frames int, budgetGB float64) VRAMPlan {
	p := VRAMPlan{
		SrcW: srcW, SrcH: srcH, Frames: frames, BudgetGB: budgetGB,
		ModelW: ceilTo(srcW, shapeQuantum), ModelH: ceilTo(srcH, shapeQuantum),
	}
	p.Megapixels = float64(p.ModelW) * float64(p.ModelH) / 1e6
	if frames <= 0 {
		p.Frames = 80 // no frame count: assume a window-filling clip
	}

	for i := len(vramChunksLow) - 1; i >= 0; i-- {
		chunk := vramChunksLow[i]
		for _, low := range []bool{false, true} {
			chunks, _ := vramTable(low)
			if !containsInt(chunks, chunk) {
				continue
			}
			window := chunk
			if p.Frames < window {
				window = p.Frames
			}
			est := estimatePeakVRAM(p.Megapixels, window, low)
			if est*vramMarginScale+vramMarginGB <= budgetGB {
				p.Chunk, p.Stride, p.LowVram, p.EstGB, p.Fits = chunk, max1(chunk/2), low, est, true
				return p
			}
		}
	}
	// Nothing fits, not even one frame at a time. Report the cheapest window
	// there is, and the resolution that would make it fit.
	p.Chunk, p.Stride, p.LowVram = 1, 1, true
	p.EstGB = estimatePeakVRAM(p.Megapixels, 1, true)
	return p
}

// fitResolution is the largest downscale of the plate that fits the budget,
// in steps of 2% and rounded to even pixels so ffmpeg and every codec after it
// are happy. It returns 0, 0 when even a thumbnail would not fit, which means
// the card is too small for this model rather than for this clip.
func fitResolution(srcW, srcH, frames int, budgetGB float64) (int, int) {
	for s := 98; s >= 10; s -= 2 {
		w := (srcW*s/100 + 1) / 2 * 2
		h := (srcH*s/100 + 1) / 2 * 2
		if w < 64 || h < 64 {
			break
		}
		if planWindow(w, h, frames, budgetGB).Fits {
			return w, h
		}
	}
	return 0, 0
}

// resolutionForWindow is the largest plate size that still gets a window of
// at least want frames. A longer window keeps the matte steadier over time,
// so when the native plate only earns a short one it is worth saying what a
// longer one would have cost. Returns 0, 0 when the native size already has
// it, or when no size does.
func resolutionForWindow(srcW, srcH, frames int, budgetGB float64, want int) (int, int) {
	if planWindow(srcW, srcH, frames, budgetGB).Chunk >= want {
		return 0, 0
	}
	for s := 95; s >= 25; s -= 5 {
		w := (srcW*s/100 + 1) / 2 * 2
		h := (srcH*s/100 + 1) / 2 * 2
		if planWindow(w, h, frames, budgetGB).Chunk >= want {
			return w, h
		}
	}
	return 0, 0
}

// Summary is the line the job log carries, so the choice is on the record next
// to the run it governed.
func (p VRAMPlan) Summary() string {
	what := fmt.Sprintf("%dx%d plate (%dx%d to the model, %.2f MP) with %.1f GB free",
		p.SrcW, p.SrcH, p.ModelW, p.ModelH, p.Megapixels, p.BudgetGB)
	if p.Fits {
		mode := "full"
		if p.LowVram {
			mode = "staged"
		}
		return fmt.Sprintf("%s: hann_chunk=%d stride=%d %s, peak ~%.1f GB of %.1f GB",
			what, p.Chunk, p.Stride, mode, p.EstGB, p.BudgetGB)
	}
	if p.FitW == 0 {
		return what + ": will not fit at any resolution — this card is too small for CorridorKey 2.0"
	}
	return fmt.Sprintf("%s: will not fit — even one frame at a time peaks at ~%.1f GB. "+
		"Resize the plate to %dx%d and it fits", what, p.EstGB, p.FitW, p.FitH)
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

// gpuFreeGB is the budget the runtime would compute for itself: free memory
// now, before anything is loaded, which is free plus reserved once it is.
// Zero means nvidia-smi could not be asked.
func gpuFreeGB() float64 {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0
	}
	// One card is what the runtime uses; the first line is the one it takes.
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	mib, err := strconv.ParseFloat(line, 64)
	if err != nil {
		return 0
	}
	return mib / 1024
}

// ffprobeBin is ffprobe next to the configured ffmpeg, which is how both
// arrive — one package, one directory.
func ffprobeBin() string {
	dir, base := filepath.Split(cfg.FFmpeg)
	return filepath.Join(dir, strings.Replace(base, "ffmpeg", "ffprobe", 1))
}

// plateInfo is what one read of a plate's header tells us.
type plateInfo struct {
	W, H   int
	Frames int     // 0 when the container does not say
	FPS    float64 // 0 when the container does not say
}

// probePlate reads the plate's size, length and frame rate. The frame count
// only matters for clips shorter than one window, so a duration times a frame
// rate is accurate enough and costs one read of the header rather than a
// decode.
func probePlate(path string) (plateInfo, error) {
	out, err := exec.Command(ffprobeBin(), "-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,nb_frames,avg_frame_rate,duration",
		"-of", "default=noprint_wrappers=1", path).Output()
	if err != nil {
		return plateInfo{}, fmt.Errorf("could not probe %s: %w", filepath.Base(path), err)
	}
	info := parseProbe(string(out))
	if info.W == 0 || info.H == 0 {
		return plateInfo{}, fmt.Errorf("%s reports no video stream size", filepath.Base(path))
	}
	return info, nil
}

// parseProbe reads ffprobe default output. nb_frames is "N/A" on plenty of
// containers, so a duration times a frame rate stands in for it.
func parseProbe(out string) plateInfo {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			fields[k] = v
		}
	}
	var info plateInfo
	info.W, _ = strconv.Atoi(fields["width"])
	info.H, _ = strconv.Atoi(fields["height"])
	info.FPS = parseRate(fields["avg_frame_rate"])
	info.Frames, _ = strconv.Atoi(fields["nb_frames"])
	if info.Frames == 0 {
		dur, _ := strconv.ParseFloat(fields["duration"], 64)
		info.Frames = int(dur * info.FPS)
	}
	return info
}

// parseRate reads ffprobe's "25/1" rational frame rate.
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	n, _ := strconv.ParseFloat(num, 64)
	if !ok {
		return n
	}
	d, _ := strconv.ParseFloat(den, 64)
	if d == 0 {
		return 0
	}
	return n / d
}
