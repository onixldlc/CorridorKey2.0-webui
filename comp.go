package main

// The only composition this UI does, and why it does it here.
//
// CorridorKey is a keyer. What it is for is the refined matte, and that is the
// one thing a key now produces: infer.py is asked for --outputs alpha and
// nothing else. Its checkerboard comp, its foreground and its cutout were
// three more EXR sequences per clip and more GPU time for pictures nobody
// delivers, so they are not requested any more.
//
// A comp is still the fastest way to see whether an edge is right, so ck-web
// builds one here from the two things it already has on disk: the plate and
// the matte. Two ffmpeg calls to get the frames as PNG, one alpha blend in
// Go, no extra sequence written anywhere.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// compMaxWidth caps a preview. A 4K matte is 8 MP of PNG per scrub step for a
// picture shown in a browser window, and the edge being judged survives the
// downscale.
const compMaxWidth = 1600

// checkerSquare is the size of one square at preview scale.
const checkerSquare = 32

// backgrounds are what the matte can be shown against. Checker is first
// because it is the one that shows a bad edge; the solids are for when the
// subject is the colour of the checker.
var backgrounds = map[string]color.NRGBA{
	"black":   {0, 0, 0, 255},
	"white":   {255, 255, 255, 255},
	"grey":    {128, 128, 128, 255},
	"green":   {0, 255, 0, 255},
	"magenta": {255, 0, 255, 255},
}

func backgroundNames() []string {
	return []string{"checker", "black", "white", "grey", "green", "magenta"}
}

// drawBackground paints the backdrop a comp sits on.
func drawBackground(w, h int, bg string) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	if c, ok := backgrounds[bg]; ok {
		draw.Draw(img, img.Bounds(), &image.Uniform{c}, image.Point{}, draw.Src)
		return img
	}
	light := color.NRGBA{180, 180, 180, 255}
	dark := color.NRGBA{140, 140, 140, 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := light
			if (x/checkerSquare+y/checkerSquare)%2 == 1 {
				c = dark
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// despillPixel is runtime.py's _despill_average_srgb_torch, one pixel at a
// time: the green over the average of red and blue is spill, and half of it is
// given back to the other two channels. The matte itself does not depend on
// despill — upstream only applies it to the foreground and its own comp — so
// with --outputs alpha the flag changes nothing on disk, and this is the only
// place the control still does anything.
func despillPixel(r, g, b float64, strength float64) (float64, float64, float64) {
	if strength <= 0 {
		return r, g, b
	}
	spill := g - (r+b)*0.5
	if spill < 0 {
		spill = 0
	}
	fr, fg, fb := r+spill*0.5, g-spill, b+spill*0.5
	if strength >= 1 {
		return fr, fg, fb
	}
	return r*(1-strength) + fr*strength,
		g*(1-strength) + fg*strength,
		b*(1-strength) + fb*strength
}

// compositeFrame renders one frame of the plate through the matte, over the
// chosen background, and returns it as PNG.
func compositeFrame(clip string, idx int, bg string, despill float64) ([]byte, error) {
	names, dir := frameList(clip, "matte")
	if len(names) == 0 {
		return nil, fmt.Errorf("no matte yet — key the clip first")
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(names) {
		idx = len(names) - 1
	}

	matteBytes, err := frameAsPNG(filepath.Join(dir, names[idx]), compMaxWidth, 0)
	if err != nil {
		return nil, fmt.Errorf("could not read matte frame %d: %w", idx, err)
	}
	matte, err := png.Decode(bytes.NewReader(matteBytes))
	if err != nil {
		return nil, fmt.Errorf("matte frame %d is not a readable png: %w", idx, err)
	}
	b := matte.Bounds()

	// The plate is scaled to the matte, not the other way around: a plate that
	// was resized to fit the card produced a smaller matte, and the matte is
	// the thing being judged.
	plateBytes, err := plateFrameAsPNG(clip, idx, b.Dx(), b.Dy())
	if err != nil {
		return nil, err
	}
	plate, err := png.Decode(bytes.NewReader(plateBytes))
	if err != nil {
		return nil, fmt.Errorf("plate frame %d is not a readable png: %w", idx, err)
	}

	out := drawBackground(b.Dx(), b.Dy(), bg)
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			// The matte is greyscale, so any channel is the coverage.
			a, _, _, _ := matte.At(b.Min.X+x, b.Min.Y+y).RGBA()
			pr, pg, pb, _ := plate.At(x, y).RGBA()
			fr, fg, fb := despillPixel(float64(pr), float64(pg), float64(pb), despill)
			base := out.NRGBAAt(x, y)
			out.SetNRGBA(x, y, color.NRGBA{
				R: mix(clamp16(fr), uint32(base.R)*257, a),
				G: mix(clamp16(fg), uint32(base.G)*257, a),
				B: mix(clamp16(fb), uint32(base.B)*257, a),
				A: 255,
			})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// clamp16 brings a channel back into range after despilling it.
func clamp16(v float64) uint32 {
	if v < 0 {
		return 0
	}
	if v > 0xffff {
		return 0xffff
	}
	return uint32(v)
}

// mix blends two 16-bit channel values by a 16-bit coverage and returns 8-bit.
func mix(fg, bg, a uint32) uint8 {
	v := (fg*a + bg*(0xffff-a)) / 0xffff
	return uint8(v >> 8)
}

// frameAsPNG converts any frame the viewer can show into PNG. EXR is
// scene-linear and no browser reads it, so it goes through ffmpeg with the
// same transfer the rest of the viewer uses; a PNG that is already the right
// size is returned untouched. maxW of 0 means full size.
func frameAsPNG(path string, maxW, maxH int) ([]byte, error) {
	vf := "scale=in_color_matrix=bt709:out_color_matrix=bt709,format=rgb24"
	switch {
	case maxW > 0 && maxH > 0:
		vf = fmt.Sprintf("scale=%d:%d,format=rgb24", maxW, maxH)
	case maxW > 0:
		// -2 keeps the aspect and an even height, and min() means a frame
		// already smaller than the cap is left alone.
		vf = fmt.Sprintf("scale='min(%d,iw)':-2,format=rgb24", maxW)
	}
	cmd := exec.Command(cfg.FFmpeg, "-v", "error", "-i", path,
		"-vf", vf, "-frames:v", "1", "-f", "image2pipe", "-c:v", "png", "-")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg could not convert %s (is it built with OpenEXR?): %w",
			filepath.Base(path), err)
	}
	return out, nil
}

// plateFrameAsPNG pulls one frame of the original plate at the size given.
// A frame-directory plate is a file on disk; a video is seeked into, which
// ffmpeg does accurately by default — it lands on the keyframe before the
// timestamp and decodes forward to it.
func plateFrameAsPNG(clip string, idx, w, h int) ([]byte, error) {
	c := inspectClip(clip)
	if c.Input == "" {
		return nil, fmt.Errorf("clip %s has no plate", clip)
	}
	if strings.HasSuffix(c.Input, "/") {
		names, dir := frameList(clip, "plate")
		if len(names) == 0 {
			return nil, fmt.Errorf("clip %s has no plate frames", clip)
		}
		if idx >= len(names) {
			idx = len(names) - 1
		}
		return frameAsPNG(filepath.Join(dir, names[idx]), w, h)
	}

	path := filepath.Join(cfg.ClipsDir, clip, c.Input)
	fps := 24.0
	if info, err := probePlate(path); err == nil && info.FPS > 0 {
		fps = info.FPS
	}
	ts := strconv.FormatFloat(float64(idx)/fps, 'f', 4, 64)
	cmd := exec.Command(cfg.FFmpeg, "-v", "error", "-accurate_seek", "-ss", ts,
		"-i", path, "-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d,format=rgb24", w, h),
		"-f", "image2pipe", "-c:v", "png", "-")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("could not read plate frame %d: %w", idx, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the plate has no frame %d", idx)
	}
	return out, nil
}

// compSource names the sequence a view counts its frames from. Only the comp
// is special: it has no folder of its own, it has the matte's length.
func compSource(view string) string {
	if view == "comp" {
		return "matte"
	}
	return view
}

// serveCompExport renders the whole comp to H.264. The preview is built frame
// by frame in Go, which is fine for one frame and far too slow for six
// hundred, so the export is one ffmpeg run: the matte becomes the alpha of the
// plate, and that goes over the background as a still.
func serveCompExport(w http.ResponseWriter, r *http.Request, clip string) {
	names, dir := frameList(clip, "matte")
	if len(names) == 0 {
		http.Error(w, "no matte yet — key the clip first", http.StatusNotFound)
		return
	}
	c := inspectClip(clip)
	if c.Input == "" {
		http.Error(w, "this clip has no plate to composite", http.StatusNotFound)
		return
	}

	// The matte decides the size: a plate resized to fit the card produced a
	// smaller matte, and the matte is what was actually keyed.
	size, err := probePlate(filepath.Join(dir, names[0]))
	if err != nil {
		http.Error(w, "could not read the matte: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fps := q(r, "fps", "")
	if fps == "" {
		fps = "24"
		if !strings.HasSuffix(c.Input, "/") {
			if info, err := probePlate(filepath.Join(cfg.ClipsDir, clip, c.Input)); err == nil && info.FPS > 0 {
				fps = strconv.FormatFloat(info.FPS, 'f', -1, 64)
			}
		}
	}

	// The background is a still PNG rather than a filter, because Go already
	// draws it for the preview and a checkerboard has no lavfi source.
	bg, err := writeBackground(clip, size.W, size.H, q(r, "bg", "checker"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(bg)

	plate := filepath.Join(cfg.ClipsDir, clip, c.Input)
	plateArgs := []string{"-i", plate}
	if strings.HasSuffix(c.Input, "/") {
		pnames, pdir := frameList(clip, "plate")
		if len(pnames) == 0 {
			http.Error(w, "this clip has no plate frames", http.StatusNotFound)
			return
		}
		plateArgs = []string{"-framerate", fps, "-pattern_type", "glob",
			"-i", filepath.Join(pdir, "*"+filepath.Ext(pnames[0]))}
	}

	size2 := fmt.Sprintf("%d:%d", size.W, size.H)
	graph := "[1:v]scale=" + size2 + ",format=rgb24" + despillFilter(despillOf(r)) + "[p];" +
		"[0:v]format=gray,scale=" + size2 + "[a];" +
		"[p][a]alphamerge[fg];" +
		"[2:v]scale=" + size2 + ",format=rgb24[bg];" +
		"[bg][fg]overlay=shortest=1[v]"

	args := append([]string{"-v", "error",
		"-framerate", fps, "-pattern_type", "glob",
		"-i", filepath.Join(dir, "*"+filepath.Ext(names[0]))}, plateArgs...)
	args = append(args,
		"-loop", "1", "-i", bg,
		"-filter_complex", graph, "-map", "[v]",
		"-r", fps,
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "18",
		"-movflags", "frag_keyframe+empty_moov", "-f", "mp4", "-")

	cmd := exec.Command(cfg.FFmpeg, args...)
	out := &deferredWriter{w: w, header: func() {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Disposition",
			"attachment; filename=\""+clip+"_comp.mp4\"")
	}}
	var errBuf bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)
	if err := cmd.Run(); err != nil {
		if out.wrote == 0 {
			http.Error(w, "ffmpeg could not write that comp: "+firstLine(errBuf.String()),
				http.StatusInternalServerError)
			return
		}
		os.Stderr.WriteString("ck-web: comp export failed mid-stream: " + err.Error() + "\n")
	}
}

// writeBackground puts the backdrop on disk for ffmpeg to read, next to the
// clip rather than in /tmp so it lands on the volume with everything else.
func writeBackground(clip string, w, h int, bg string) (string, error) {
	dir := filepath.Join(cfg.ClipsDir, clip, "Output")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".bg-*.png")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := png.Encode(f, drawBackground(w, h, bg)); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// despillOf reads the preview's despill control off the query string, as the
// 0-10 the slider sends.
func despillOf(r *http.Request) float64 {
	v, err := strconv.Atoi(q(r, "despill", "5"))
	if err != nil {
		return 0.5
	}
	return Params{Despill: v}.despillStrength()
}

// despillFilter is despillPixel as a geq, for the export, where doing it a
// pixel at a time in Go would mean six hundred round trips. Empty at zero
// strength, so the plate is passed through untouched.
func despillFilter(strength float64) string {
	if strength <= 0 {
		return ""
	}
	s := strconv.FormatFloat(strength, 'f', 3, 64)
	spill := "max(g(X,Y)-(r(X,Y)+b(X,Y))/2,0)"
	return ",geq=" +
		"r='r(X,Y)+" + s + "*0.5*" + spill + "':" +
		"g='g(X,Y)-" + s + "*" + spill + "':" +
		"b='b(X,Y)+" + s + "*0.5*" + spill + "'"
}
