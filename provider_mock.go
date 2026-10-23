package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// MockProvider keys without a model: a per-pixel green-excess matte, written
// in the same folder layout the real provider produces.
//
// It is here so a dev run exercises the whole app — upload, queue, live log,
// viewer, scrubber, export — on a laptop with no GPU, no Python and nothing
// downloaded. The mattes it makes are not production work and the UI says so.
type MockProvider struct{}

func (MockProvider) ID() string             { return "mock" }
func (MockProvider) Title() string          { return "Mock keyer (no model, dev only)" }
func (MockProvider) NeedsModules() []string { return nil }
func (MockProvider) Ready() (bool, string) {
	if _, err := exec.LookPath(cfg.FFmpeg); err != nil {
		return true, "ready — no ffmpeg, so plates are replaced by a synthetic test pattern"
	}
	return true, "ready — plain green-excess key, no model involved"
}

const mockSynthFrames = 24

func (m MockProvider) Run(ctx context.Context, action, clip string, p Params, log func(string)) error {
	clips, err := m.targets(clip, action)
	if err != nil {
		return err
	}
	if len(clips) == 0 {
		log("No clips need " + action + ".")
		return nil
	}
	for _, c := range clips {
		if err := ctx.Err(); err != nil {
			return err
		}
		if action == "generate_alphas" {
			if err := m.alphas(ctx, c, log); err != nil {
				return err
			}
			continue
		}
		if err := m.key(ctx, c, p, log); err != nil {
			return err
		}
	}
	log("Mock run complete. These mattes are a stand-in — rent the GPU and switch to the corridorkey provider for real ones.")
	return nil
}

// targets mirrors which clips the real CLI would pick up for an action.
func (MockProvider) targets(clip, action string) ([]string, error) {
	all, err := listClips()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range all {
		if clip != "" && c.Name != clip {
			continue
		}
		switch action {
		case "generate_alphas":
			if c.Stage == StageNeedsAlpha {
				out = append(out, c.Name)
			}
		case "run_inference":
			// Anything with a plate. A hint is an optional input in 2.0, so
			// waiting for one here would contradict the Clips page, which
			// calls a clip with a plate alone ready to key.
			if c.Stage != StageEmpty {
				out = append(out, c.Name)
			}
		}
	}
	if clip != "" && len(out) == 0 {
		return nil, fmt.Errorf("clip %q is not at a stage %s can act on", clip, action)
	}
	return out, nil
}

// alphas writes a coarse hint: the green-excess matte, blurred and widened the
// way a hint is meant to be loose around the edge.
func (m MockProvider) alphas(ctx context.Context, clip string, log func(string)) error {
	frames, cleanup, err := m.plate(ctx, clip, log)
	if err != nil {
		return err
	}
	defer cleanup()

	dir := filepath.Join(cfg.ClipsDir, clip, "AlphaHint")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return err
	}
	log(fmt.Sprintf("Generating hint for %s — %d frames", clip, len(frames)))
	for i, f := range frames {
		if err := ctx.Err(); err != nil {
			return err
		}
		src, err := loadPNG(f)
		if err != nil {
			return err
		}
		a := greenExcess(src, 1.0)
		blur(a, 3)
		widen(a)
		out := filepath.Join(dir, fmt.Sprintf("%s_alphaHint_%04d.png", clip, i))
		if err := savePNG(out, a); err != nil {
			return err
		}
		if i%10 == 0 || i == len(frames)-1 {
			log(fmt.Sprintf("  hint %d/%d", i+1, len(frames)))
		}
	}
	log("Saved " + fmt.Sprint(len(frames)) + " alpha frames to " + dir)
	return nil
}

// key writes a matte, and only a matte, because that is all the real provider
// asks the runtime for. The preview the viewer shows over a checkerboard is
// composited from this and the plate on request, the same way it is for a real
// key, so a dev run exercises the same path.
func (m MockProvider) key(ctx context.Context, clip string, p Params, log func(string)) error {
	frames, cleanup, err := m.plate(ctx, clip, log)
	if err != nil {
		return err
	}
	defer cleanup()

	matteDir := filepath.Join(cfg.ClipsDir, clip, "Output", "Matte")
	if err := os.MkdirAll(matteDir, 0o775); err != nil {
		return err
	}
	log(fmt.Sprintf("Running mock inference on: %s — %d frames, refiner %.2f",
		clip, len(frames), p.Refiner))

	for i, f := range frames {
		if err := ctx.Err(); err != nil {
			return err
		}
		src, err := loadPNG(f)
		if err != nil {
			return err
		}
		matte := greenExcess(src, 1.0+p.Refiner)
		if p.Despeckle {
			despeckle(matte, p.SpeckSize)
		}
		stem := fmt.Sprintf("%s_%04d", clip, i)
		if err := savePNG(filepath.Join(matteDir, stem+".png"), matte); err != nil {
			return err
		}
		if i%10 == 0 || i == len(frames)-1 {
			log(fmt.Sprintf("  frame %d/%d", i+1, len(frames)))
		}
	}
	log("Inference Complete.")
	return nil
}

// plate returns PNG frames of the clip's input: extracted with ffmpeg if it is
// installed and the plate is a video, read straight off disk if the clip holds
// a sequence, or synthesised when there is nothing to read.
func (m MockProvider) plate(ctx context.Context, clip string, log func(string)) ([]string, func(), error) {
	noop := func() {}
	dir := filepath.Join(cfg.ClipsDir, clip)
	c := inspectClip(clip)

	if strings.HasSuffix(c.Input, "/") {
		names, d := frameList(clip, "input")
		var out []string
		for _, n := range names {
			if strings.EqualFold(filepath.Ext(n), ".png") {
				out = append(out, filepath.Join(d, n))
			}
		}
		if len(out) > 0 {
			return out, noop, nil
		}
	}

	if c.Input != "" && !strings.HasSuffix(c.Input, "/") {
		if _, err := exec.LookPath(cfg.FFmpeg); err == nil {
			tmp, err := os.MkdirTemp(dir, ".plate-")
			if err != nil {
				return nil, noop, err
			}
			log("Extracting the plate with ffmpeg")
			cmd := exec.CommandContext(ctx, cfg.FFmpeg, "-v", "error", "-i", filepath.Join(dir, c.Input),
				"-vf", "scale=min(1280\\,iw):-2", filepath.Join(tmp, "f%05d.png"))
			if err := runLogged(cmd, log); err != nil {
				os.RemoveAll(tmp)
				return nil, noop, fmt.Errorf("ffmpeg could not read the plate: %w", err)
			}
			entries, _ := os.ReadDir(tmp)
			var out []string
			for _, e := range entries {
				out = append(out, filepath.Join(tmp, e.Name()))
			}
			sort.Strings(out)
			if len(out) == 0 {
				os.RemoveAll(tmp)
				return nil, noop, fmt.Errorf("ffmpeg produced no frames")
			}
			return out, func() { os.RemoveAll(tmp) }, nil
		}
		log("No ffmpeg here — using a synthetic test pattern instead of the plate")
	}

	tmp, err := os.MkdirTemp(dir, ".plate-")
	if err != nil {
		return nil, noop, err
	}
	var out []string
	for i := 0; i < mockSynthFrames; i++ {
		p := filepath.Join(tmp, fmt.Sprintf("f%05d.png", i))
		if err := savePNG(p, synthFrame(i, mockSynthFrames)); err != nil {
			os.RemoveAll(tmp)
			return nil, noop, err
		}
		out = append(out, p)
	}
	return out, func() { os.RemoveAll(tmp) }, nil
}

// ── the pixels ──────────────────────────────────────────────────────────────

// greenExcess is the whole "model": how much greener a pixel is than its own
// red and blue. Opaque where the subject is, transparent on the screen.
func greenExcess(src image.Image, gain float64) *image.Gray {
	b := src.Bounds()
	out := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := src.At(x, y).RGBA()
			rf, gf, bf := float64(r>>8), float64(g>>8), float64(bl>>8)
			spill := (gf - math.Max(rf, bf)) * 2.2 * gain
			a := 255.0 - clamp(spill, 0, 255)
			out.SetGray(x, y, color.Gray{Y: uint8(a)})
		}
	}
	return out
}

// despeckle drops isolated specks, the way the CLI's auto-despeckle does.
// minPixels is compared against a 3x3 neighbourhood mean rather than a real
// connected-component size: this is a stand-in, not the real filter.
func despeckle(m *image.Gray, minPixels int) {
	if minPixels <= 0 {
		return
	}
	b := m.Bounds()
	cp := image.NewGray(b)
	copy(cp.Pix, m.Pix)
	thresh := uint8(clamp(float64(minPixels)/8, 16, 200))
	for y := b.Min.Y + 1; y < b.Max.Y-1; y++ {
		for x := b.Min.X + 1; x < b.Max.X-1; x++ {
			var sum int
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					sum += int(cp.GrayAt(x+dx, y+dy).Y)
				}
			}
			mean := sum / 9
			if cp.GrayAt(x, y).Y > thresh && mean < int(thresh) {
				m.SetGray(x, y, color.Gray{Y: 0})
			}
		}
	}
}

// blur is a separable box blur, run once per axis.
func blur(m *image.Gray, radius int) {
	if radius < 1 {
		return
	}
	b := m.Bounds()
	tmp := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			var sum, n int
			for dx := -radius; dx <= radius; dx++ {
				if x+dx < b.Min.X || x+dx >= b.Max.X {
					continue
				}
				sum += int(m.GrayAt(x+dx, y).Y)
				n++
			}
			tmp.SetGray(x, y, color.Gray{Y: uint8(sum / n)})
		}
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			var sum, n int
			for dy := -radius; dy <= radius; dy++ {
				if y+dy < b.Min.Y || y+dy >= b.Max.Y {
					continue
				}
				sum += int(tmp.GrayAt(x, y+dy).Y)
				n++
			}
			m.SetGray(x, y, color.Gray{Y: uint8(sum / n)})
		}
	}
}

// widen pushes the hint outward, since a hint is meant to be loose.
func widen(m *image.Gray) {
	for i, v := range m.Pix {
		if v > 8 {
			m.Pix[i] = uint8(clamp(float64(v)*1.4, 0, 255))
		}
	}
}

// synthFrame is the stand-in plate: a green screen, a moving subject and a
// couple of tracking dots, so every control in the UI visibly does something.
func synthFrame(i, n int) image.Image {
	const w, h = 640, 360
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	phase := float64(i) / float64(n) * 2 * math.Pi
	cx := w/2 + int(math.Sin(phase)*90)
	cy := h/2 + int(math.Cos(phase)*30)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// An uneven screen, as a real one is.
			g := 150 + int(30*math.Sin(float64(x)/90)) + int(20*math.Cos(float64(y)/60))
			c := color.RGBA{uint8(26 + g/9), uint8(clamp(float64(g), 0, 255)), uint8(30 + g/11), 255}
			dx, dy := float64(x-cx)/86, float64(y-cy)/120
			if dx*dx+dy*dy < 1 {
				shade := uint8(clamp(170+40*math.Sin(float64(y)/18), 0, 255))
				c = color.RGBA{shade, uint8(float64(shade) * 0.62), uint8(float64(shade) * 0.52), 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	for _, d := range [][2]int{{70, 60}, {560, 300}, {520, 70}} {
		for y := d[1] - 3; y <= d[1]+3; y++ {
			for x := d[0] - 3; x <= d[0]+3; x++ {
				img.SetRGBA(x, y, color.RGBA{210, 40, 60, 255})
			}
		}
	}
	return img
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func loadPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

func savePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
