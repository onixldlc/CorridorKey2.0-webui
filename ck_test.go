package main

import (
	"fmt"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// safeName is the only thing between a request path and the filesystem.
func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"Shot_010":         "Shot_010",
		"../../etc/passwd": "passwd",
		"a/b":              "b",
		"":                 "",
		".":                "",
		"  spaced  ":       "spaced",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"../../etc/passwd", "a/b", "..", "./.."} {
		if got := safeName(in); strings.Contains(got, string(filepath.Separator)) || got == ".." {
			t.Errorf("safeName(%q) = %q, which still escapes", in, got)
		}
	}
}

// Every module must declare where it comes from and what proves it arrived,
// or the Modules page shows a row that can never turn green.
func TestModulesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range modules {
		if seen[m.Key] {
			t.Errorf("duplicate module key %q", m.Key)
		}
		seen[m.Key] = true
		if m.Marker == "" || m.Dest == "" || m.Title == "" {
			t.Errorf("module %q is missing a title, dest or marker", m.Key)
		}
		if m.Repo == "" && len(m.Files) == 0 {
			t.Errorf("module %q has no source", m.Key)
		}
		if !strings.HasPrefix(m.Source, "https://") {
			t.Errorf("module %q source is not an https page: %q", m.Key, m.Source)
		}
	}
	if moduleByKey("corridorkey2") == nil {
		t.Fatal("the CorridorKey 2.0 keyer is not in the registry")
	}
}

// A dev run must never be able to start a download.
func TestDevModeBlocksInstall(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.Dev = true
	if err := install(nil, "corridorkey", func(string) {}); err == nil {
		t.Fatal("install succeeded in dev mode")
	}
}

// TestPageURL pins the link printed at startup: it is the only place the
// operator learns the token, so the wildcard addresses have to come out as
// something a browser will accept.
func TestPageURL(t *testing.T) {
	for _, c := range []struct{ addr, token, want string }{
		{":8080", "abc", "http://localhost:8080/?t=abc"},
		{"0.0.0.0:8080", "abc", "http://localhost:8080/?t=abc"},
		{"[::]:8080", "", "http://localhost:8080/"},
		{"gpu-box:9000", "a b", "http://gpu-box:9000/?t=a+b"},
		{"127.0.0.1:8080", "", "http://127.0.0.1:8080/"},
	} {
		if got := pageURL(c.addr, c.token); got != c.want {
			t.Errorf("pageURL(%q, %q) = %q, want %q", c.addr, c.token, got, c.want)
		}
	}
}

// TestParseAria2Progress pins the summary format: if aria2 changes it the
// fallback is a silent install log, not a failure, so only a test catches it.
func TestParseAria2Progress(t *testing.T) {
	ok := []struct{ line, want string }{
		{"[#2089b0 400MiB/2.5GiB(15%) CN:16 DL:85MiB ETA:25s]",
			"400MiB / 2.5GiB (15%) · 85MiB/s · 16 conn · ETA 25s"},
		{"[#a1b2c3 1.2GiB/37GiB(3%) CN:16 DL:112MiB ETA:5m30s]",
			"1.2GiB / 37GiB (3%) · 112MiB/s · 16 conn · ETA 5m30s"},
		{"[#ff0001 383MiB/383MiB(100%) CN:1 DL:0B]",
			"383MiB / 383MiB (100%) · 0B/s · 1 conn"},
	}
	for _, c := range ok {
		got, found := parseAria2Progress(c.line)
		if !found {
			t.Errorf("parseAria2Progress(%q) did not match", c.line)
			continue
		}
		if got != c.want {
			t.Errorf("parseAria2Progress(%q) = %q, want %q", c.line, got, c.want)
		}
	}
	for _, line := range []string{
		"",
		"10/22 00:00:00 [NOTICE] Download complete: /data/x.safetensors",
		"[ERROR] CUID#7 - Download aborted. URI=https://huggingface.co/x",
	} {
		if got, found := parseAria2Progress(line); found {
			t.Errorf("parseAria2Progress(%q) matched as progress: %q", line, got)
		}
	}
}

// TestAria2ConnCapIsAria2sOwnLimit guards the constant: aria2c exits with an
// option error above 16, which would fail the install rather than slow it.
func TestAria2ConnCapIsAria2sOwnLimit(t *testing.T) {
	if aria2MaxConn < 1 || aria2MaxConn > 16 {
		t.Fatalf("aria2MaxConn = %d, aria2 accepts 1..16", aria2MaxConn)
	}
}

// TestDespillStrength pins the mapping from the UI's 0-10 slider onto
// infer.py's 0.0-1.0 --despill_strength, including the clamps.
func TestDespillStrength(t *testing.T) {
	for _, c := range []struct {
		in   int
		want float64
	}{{0, 0}, {5, 0.5}, {10, 1}, {-3, 0}, {25, 1}} {
		if got := (Params{Despill: c.in}).despillStrength(); got != c.want {
			t.Errorf("despillStrength(%d) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestModuleRevisionsArePinned guards the two foundation models whose exact
// revision the runtime depends on: a float to "main" would load the wrong
// tensors and fail somewhere far from here.
func TestModuleRevisionsArePinned(t *testing.T) {
	want := map[string]string{
		"cradio": "c0457f5dc26ca145f954cd4fc5bb6114e5705ad8",
		"moge":   "ca5f0e07ff01d3e5a364c1d954ed12ee1814b368",
	}
	for key, rev := range want {
		m := moduleByKey(key)
		if m == nil {
			t.Fatalf("module %q is gone", key)
		}
		if m.Rev != rev {
			t.Errorf("module %q pinned at %q, want %q", key, m.Rev, rev)
		}
		if got := hfURL(m.Repo, m.Rev, "model.safetensors"); !strings.Contains(got, "/resolve/"+rev+"/") {
			t.Errorf("hfURL ignored the pin: %s", got)
		}
	}
}

// TestEveryModuleIsRequired is a statement of fact about 2.0: unlike the 1.x
// line there is nothing optional to pick from, so a missing one is fatal.
func TestEveryModuleIsRequired(t *testing.T) {
	if len(modules) != 4 {
		t.Fatalf("expected the 4 CorridorKey 2.0 weights, got %d", len(modules))
	}
	for _, m := range modules {
		if !m.Required {
			t.Errorf("module %q is not marked required", m.Key)
		}
	}
}

// TestExcludeDropsTheDuplicateCradioWeight — C-RADIO ships a .pth.tar that
// duplicates model.safetensors; fetching it wastes 1.2 GB per install.
func TestExcludeDropsTheDuplicateCradioWeight(t *testing.T) {
	m := moduleByKey("cradio")
	if m == nil {
		t.Fatal("cradio module is gone")
	}
	if !m.skip("c-radio_v4-so400m_half.pth.tar") {
		t.Error("the duplicate .pth.tar would be downloaded")
	}
	if m.skip("model.safetensors") {
		t.Error("the weight the runtime actually reads would be skipped")
	}
}

// TestEveryModuleDeclaresWhatItCosts — the provisioning gate adds these up to
// decide whether the volume can hold what is missing. A zero would make a
// full disk look free.
func TestEveryModuleDeclaresWhatItCosts(t *testing.T) {
	for _, m := range modules {
		if m.Need <= 0 {
			t.Errorf("module %q does not declare Need", m.Key)
		}
	}
}

// TestProvisionDecision pins the two gates the operator asked for — a GPU and
// the disk to finish — plus the cases where self-provisioning must stay out of
// the way.
func TestProvisionDecision(t *testing.T) {
	ok := Provision{Mode: "auto", GPUs: 1, Missing: []string{"corridorkey2"},
		Disks: []DiskNeed{{What: "weights", Path: "/data", Need: 10 << 30, Free: 60 << 30}}}

	if auto, why := provisionDecision(ok); !auto {
		t.Errorf("a GPU box with room declined to provision: %s", why)
	}

	noGPU := ok
	noGPU.GPUs = 0
	if auto, _ := provisionDecision(noGPU); auto {
		t.Error("provisioned a box with no GPU")
	}

	full := ok
	full.Disks = []DiskNeed{{What: "weights", Path: "/data", Need: 10 << 30, Free: 9 << 30}}
	if auto, _ := provisionDecision(full); auto {
		t.Error("provisioned a box that cannot hold the download")
	}

	unknown := ok
	unknown.Disks = []DiskNeed{{What: "weights", Path: "/data", Need: 1, Free: -1}}
	if auto, _ := provisionDecision(unknown); auto {
		t.Error("provisioned a box whose free space could not be read")
	}

	dev := ok
	dev.Mode = "dev"
	if auto, _ := provisionDecision(dev); auto {
		t.Error("a dev run started a download")
	}

	manual := ok
	manual.Mode = "manual"
	if auto, _ := provisionDecision(manual); auto {
		t.Error("-mode manual started a download")
	}

	done := Provision{Mode: "auto", GPUs: 1, Stack: true, Missing: []string{}}
	if auto, why := provisionDecision(done); auto {
		t.Errorf("re-provisioned an already complete box: %s", why)
	}
}

// TestFoldNeedsSumsOneFilesystem — in the image the stack, the weights and the
// clips all land on /data. Checking each against the same free-space figure
// would pass a disk that cannot hold the three together.
func TestFoldNeedsSumsOneFilesystem(t *testing.T) {
	dir := t.TempDir()
	got := foldNeeds([]DiskNeed{
		{What: "a", Path: dir, Need: 1 << 30},
		{What: "b", Path: filepath.Join(dir, "sub", "deeper"), Need: 2 << 30},
	})
	if len(got) != 1 {
		t.Fatalf("two targets on one filesystem folded into %d entries", len(got))
	}
	if got[0].Need != 3<<30 {
		t.Errorf("folded need = %d, want %d", got[0].Need, int64(3)<<30)
	}
	if got[0].Free <= 0 {
		t.Errorf("free space on %s came back as %d", dir, got[0].Free)
	}
}

// TestProvisionPlanIsReadOnly guards the startup path: measuring the box must
// never be what creates the directories it is measuring.
func TestProvisionPlanIsReadOnly(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.Root = filepath.Join(t.TempDir(), "nothing-here")
	cfg.ClipsDir = filepath.Join(cfg.Root, "ClipsForInference")
	cfg.Python = filepath.Join(cfg.Root, "venv", "bin", "python")

	p := planProvision()
	if _, err := os.Stat(cfg.Root); err == nil {
		t.Error("planProvision created the data root")
	}
	if p.Stack {
		t.Error("an empty root reported the inference stack as present")
	}
	if len(p.Missing) != len(modules) {
		t.Errorf("an empty root is missing %d modules, want %d", len(p.Missing), len(modules))
	}
	for _, d := range p.Disks {
		if d.Free <= 0 {
			t.Errorf("%s: free space %d — statfs did not walk up to an existing parent", d.Path, d.Free)
		}
	}
}

// TestVRAMTableMatchesUpstream pins the transcription of _VRAM_PEAK_GB: the
// whole window decision rests on these numbers, and a typo in one of them
// would only ever show up as an out-of-memory twenty minutes into a key.
func TestVRAMTableMatchesUpstream(t *testing.T) {
	for _, c := range []struct {
		mp    float64
		chunk int
		low   bool
		want  float64
	}{
		{0.98, 8, false, 6.19},
		{8.36, 80, false, 70.45},
		{3.93, 48, false, 23.92},
		{0.98, 1, true, 2.71},
		{8.36, 24, true, 20.78},
		{2.21, 4, true, 3.35},
	} {
		if got := estimatePeakVRAM(c.mp, c.chunk, c.low); math.Abs(got-c.want) > 0.005 {
			t.Errorf("estimatePeakVRAM(%v, %d, %v) = %.2f, want %.2f",
				c.mp, c.chunk, c.low, got, c.want)
		}
	}
}

// TestVRAMPlanRejectsTheWindowThatRanOut replays the clip that failed: a
// 3840x2160 plate, 348 frames, 23.3 GB free on a 3090. The runtime sized
// itself a 24-frame window there, cleared its own margin by 0.14 GB, and died
// on "tried to allocate 2.24 GiB". Whatever this picks, it must not be that.
func TestVRAMPlanRejectsTheWindowThatRanOut(t *testing.T) {
	p := planVRAM(3840, 2160, 348, 23.3)
	if !p.Fits {
		t.Fatalf("4K on a 24 GB card should still key: %s", p.Summary())
	}
	if p.ModelW != 3840 || p.ModelH != 2176 {
		t.Errorf("model input is %dx%d, want 3840x2176 (padded to %d)",
			p.ModelW, p.ModelH, shapeQuantum)
	}
	if p.Chunk >= 24 {
		t.Errorf("picked hann_chunk=%d, which is the window that ran out", p.Chunk)
	}
	if got := p.EstGB*vramMarginScale + vramMarginGB; got > p.BudgetGB {
		t.Errorf("plan needs %.1f GB with the margin, budget is %.1f GB", got, p.BudgetGB)
	}
	// And the margin has to be the thing that rejected it, not luck.
	if estimatePeakVRAM(p.Megapixels, 24, true)*1.1+0.3 > 23.3 {
		t.Error("upstream margin no longer accepts chunk 24 here, so this test " +
			"is no longer about the clip that failed")
	}
}

// TestVRAMPlanFindsAResolutionThatFits — between 4.16 and 4.89 GB free, no
// window fits a 4K plate but one fits a smaller one, which is the only case
// where resizing the plate is the right answer.
func TestVRAMPlanFindsAResolutionThatFits(t *testing.T) {
	p := planVRAM(3840, 2160, 348, 4.5)
	if p.Fits {
		t.Fatalf("4K should not fit in 4.5 GB: %s", p.Summary())
	}
	if p.FitW == 0 || p.FitW >= 3840 {
		t.Fatalf("no smaller plate was suggested: %s", p.Summary())
	}
	if p.FitW%2 != 0 || p.FitH%2 != 0 {
		t.Errorf("suggested %dx%d, which no encoder will take", p.FitW, p.FitH)
	}
	if fitted := planVRAM(p.FitW, p.FitH, 348, 4.5); !fitted.Fits {
		t.Errorf("the suggested %dx%d does not itself fit: %s", p.FitW, p.FitH, fitted.Summary())
	}
	if !strings.Contains(p.Summary(), "Resize the plate") {
		t.Errorf("the log line does not say what to do: %s", p.Summary())
	}
}

// TestVRAMPlanGivesUpOnATooSmallCard: below the smallest measured peak there
// is no resolution to retreat to, and saying so beats suggesting 64x36.
func TestVRAMPlanGivesUpOnATooSmallCard(t *testing.T) {
	p := planVRAM(1920, 1080, 100, 2.0)
	if p.Fits || p.FitW != 0 {
		t.Fatalf("2 GB should be hopeless, got: %s", p.Summary())
	}
	if !strings.Contains(p.Summary(), "too small") {
		t.Errorf("summary should name the card as the problem: %s", p.Summary())
	}
}

// TestCK2TrackerFollowsThePhases walks the output of a real key and checks
// the bar lands where the log says it is.
func TestCK2TrackerFollowsThePhases(t *testing.T) {
	j := &Job{ID: 1}
	tr := newCK2Tracker(j, t.TempDir())
	for _, line := range []string{
		"Loading CorridorKey v2 runtime weights: /data/weights/corridorkey_v2.safetensors",
		"  probed 348 frames @ 3840x2176 (native 3840x2160, fps 25.00, start=0, pad=(0, 0, 8, 8))",
		"    RVM continuous prepass: T=348, chunk=8, input=960x544, grid=60x34",
	} {
		tr.line(line)
	}
	if j.Progress.Step != ckPhaseRVM || j.Progress.Phase != "RVM prepass" {
		t.Fatalf("after the prepass line the phase is %+v", j.Progress)
	}
	if tr.frames != 348 {
		t.Errorf("frame count is %d, want 348", tr.frames)
	}

	tr.line("    frozen VFM prepass: T=348, grid=60x34, chunk=4")
	if j.Progress.Step != ckPhaseVFM {
		t.Errorf("phase is %d, want the foundation prepass", j.Progress.Step)
	}

	tr.line("    Hann stage1: chunk=8, stride=4, windows=87, baton_grid=60x34")
	if j.Progress.Step != ckPhaseKey || tr.windows != 87 {
		t.Fatalf("stage1 was not picked up: step %d, windows %d", j.Progress.Step, tr.windows)
	}

	// publish() rate-limits, so the phase change above is the last push; the
	// window lines below are forced through by asking the tracker directly.
	tr.line("      window 44/87 [172:180]")
	tr.publish(true)
	if j.Progress.Pct != 50 {
		t.Errorf("window 44 of 87 reads as %d%%, want 50%%", j.Progress.Pct)
	}
	if !strings.Contains(j.Progress.Note, "frame 180 of 348") {
		t.Errorf("note does not say where in the clip it is: %q", j.Progress.Note)
	}
}

// TestSparseFillMeasuresWhatIsWritten — the prepasses print nothing, so the
// bar comes from how much of each memmap the filesystem has blocks for.
func TestSparseFillMeasuresWhatIsWritten(t *testing.T) {
	dir := t.TempDir()
	if got := sparseFill(dir, "*_rvm_feat.f16"); got != -1 {
		t.Errorf("an empty directory reads as %d%%, want -1 (unknown)", got)
	}
	f, err := os.Create(filepath.Join(dir, "Shot_010_rvm_feat.f16"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(8 << 20); err != nil { // the full memmap, none of it written
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := sparseFill(dir, "*_rvm_feat.f16"); got < 0 || got > 10 {
		t.Errorf("a barely-written 8 MiB memmap reads as %d%%, want near 0 "+
			"(or -1 on a filesystem without sparse files)", got)
	}
}

// TestParseProbeReadsRealFfprobeOutput pins both shapes ffprobe actually
// answers with, captured from the image itself: an mp4 that knows its length,
// and a matroska file that answers N/A to both the frame count and the
// duration. The second is why a missing frame count cannot be an error.
func TestParseProbeReadsRealFfprobeOutput(t *testing.T) {
	info := parseProbe("width=3840\nheight=2160\navg_frame_rate=25/1\n" +
		"duration=2.000000\nnb_frames=50\n")
	if info.W != 3840 || info.H != 2160 || info.Frames != 50 || info.FPS != 25 {
		t.Errorf("mp4 read as %dx%d, %d frames at %v fps", info.W, info.H, info.Frames, info.FPS)
	}

	info = parseProbe("width=1920\nheight=1080\navg_frame_rate=24/1\n" +
		"duration=N/A\nnb_frames=N/A\n")
	if info.W != 1920 || info.H != 1080 {
		t.Errorf("mkv read as %dx%d", info.W, info.H)
	}
	if info.Frames != 0 {
		t.Errorf("frame count is %d, want 0 — unknown, not invented", info.Frames)
	}
	// Unknown length must still plan, as a clip longer than any window.
	if p := planVRAM(info.W, info.H, info.Frames, 23.3); !p.Fits || p.Frames != 80 {
		t.Errorf("a clip of unknown length planned as %+v", p)
	}
}

// TestEveryViewHasSomethingBehindIt — the viewer offers four tabs and three
// of them are folders. The fourth is rendered, so it must NOT resolve to one,
// or an empty directory would read as "no frames" instead of a comp.
func TestEveryViewHasSomethingBehindIt(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.ClipsDir = "/clips"
	for _, view := range []string{"plate", "hint", "matte"} {
		if viewDir("Shot_010", view) == "" {
			t.Errorf("view %q maps to no folder", view)
		}
	}
	if viewDir("Shot_010", "comp") != "" {
		t.Error("the comp has a folder, but nothing writes one")
	}
	if got := compSource("comp"); got != "matte" {
		t.Errorf("a comp counts %q frames, want the matte's", got)
	}
	if got := compSource("matte"); got != "matte" {
		t.Errorf("compSource rewrote %q", got)
	}
	// The old names still have to answer: a page left open on v0.1.6 asks for
	// them, and a dead tab is worse than a stale one.
	if viewDir("Shot_010", "alpha") != viewDir("Shot_010", "hint") {
		t.Error("the old alpha view no longer finds the hint folder")
	}
	if viewDir("Shot_010", "input") != viewDir("Shot_010", "plate") {
		t.Error("the old input view no longer finds the plate folder")
	}
}

// TestOnlyTheMatteIsAskedFor pins the argument that made three sequences and
// a chunk of GPU time go away.
func TestOnlyTheMatteIsAskedFor(t *testing.T) {
	if len(ck2Views) != 1 || ck2Views[0].stage != "Matte" {
		t.Fatalf("the collector handles %+v, want the matte alone", ck2Views)
	}
}

// TestBackgroundsAreDrawn: the checkerboard has to alternate, and a flat
// colour has to be flat. An unknown name falls back to the checker rather
// than to a black frame that looks like a failed render.
func TestBackgroundsAreDrawn(t *testing.T) {
	ck := drawBackground(96, 96, "checker")
	a := ck.NRGBAAt(0, 0)
	b := ck.NRGBAAt(checkerSquare, 0)
	if a == b {
		t.Error("the checkerboard does not alternate across a square")
	}
	if got := ck.NRGBAAt(checkerSquare, checkerSquare); got != a {
		t.Errorf("the diagonal square is %v, want the same as the first (%v)", got, a)
	}
	if got := drawBackground(8, 8, "green").NRGBAAt(4, 4); got != (color.NRGBA{0, 255, 0, 255}) {
		t.Errorf("green is %v", got)
	}
	if got := drawBackground(8, 8, "nonsense").NRGBAAt(0, 0); got != a {
		t.Errorf("an unknown background drew %v, want the checker", got)
	}
	for _, name := range backgroundNames() {
		if name == "checker" {
			continue
		}
		if _, ok := backgrounds[name]; !ok {
			t.Errorf("the UI offers %q and the server cannot draw it", name)
		}
	}
}

// TestMixIsStraightAlpha — the comp is one line of arithmetic, and getting it
// wrong looks like a bad matte rather than a bad blend.
func TestMixIsStraightAlpha(t *testing.T) {
	const fg, bg = 0xffff, 0x0000
	if got := mix(fg, bg, 0xffff); got != 255 {
		t.Errorf("full coverage gave %d, want the foreground", got)
	}
	if got := mix(fg, bg, 0); got != 0 {
		t.Errorf("no coverage gave %d, want the background", got)
	}
	if got := mix(fg, bg, 0x8000); got < 126 || got > 130 {
		t.Errorf("half coverage gave %d, want about 128", got)
	}
}

// TestDespillMatchesUpstream — the preview's despill is a transcription of
// runtime.py's _despill_average_srgb_torch, so it is pinned against the
// arithmetic there: the green above the mean of red and blue is spill, and
// half of it goes back to the other two channels.
func TestDespillMatchesUpstream(t *testing.T) {
	const r, g, b = 0.2, 0.8, 0.2 // spill of 0.6
	gr, gg, gb := despillPixel(r, g, b, 1)
	if math.Abs(gr-0.5) > 1e-9 || math.Abs(gg-0.2) > 1e-9 || math.Abs(gb-0.5) > 1e-9 {
		t.Errorf("full despill gave (%.3f, %.3f, %.3f), want (0.5, 0.2, 0.5)", gr, gg, gb)
	}
	if hr, hg, hb := despillPixel(r, g, b, 0); hr != r || hg != g || hb != b {
		t.Errorf("zero despill changed the pixel to (%.3f, %.3f, %.3f)", hr, hg, hb)
	}
	hr, hg, hb := despillPixel(r, g, b, 0.5)
	if math.Abs(hr-0.35) > 1e-9 || math.Abs(hg-0.5) > 1e-9 || math.Abs(hb-0.35) > 1e-9 {
		t.Errorf("half despill gave (%.3f, %.3f, %.3f), want (0.35, 0.5, 0.35)", hr, hg, hb)
	}
	// A pixel with no green excess must come back untouched at any strength.
	if cr, cg, cb := despillPixel(0.9, 0.1, 0.9, 1); cr != 0.9 || cg != 0.1 || cb != 0.9 {
		t.Errorf("a pixel with no spill was changed to (%.3f, %.3f, %.3f)", cr, cg, cb)
	}

	// The export does the same thing in ffmpeg, and must agree about zero.
	if got := despillFilter(0); got != "" {
		t.Errorf("zero despill still built a filter: %q", got)
	}
	if got := despillFilter(0.5); !strings.Contains(got, "geq=") ||
		!strings.Contains(got, "max(g(X,Y)-(r(X,Y)+b(X,Y))/2,0)") {
		t.Errorf("the export filter does not carry the same spill term: %q", got)
	}
}

// TestFrameCacheEvictsOldest — the LRU is what keeps a long clip from growing
// without bound, so its ceiling and its order are both pinned.
func TestFrameCacheEvictsOldest(t *testing.T) {
	c := newFrameCache(300)
	put := func(k string, n int) {
		c.put(k, renderedFrame{Body: make([]byte, n), Cache: true})
	}
	put("a", 100)
	put("b", 100)
	put("c", 100)
	if _, ok := c.get("a"); !ok {
		t.Fatal("a fell out while there was still room")
	}
	// a was just read, so b is now the oldest and must go first.
	put("d", 100)
	if _, ok := c.get("b"); ok {
		t.Error("the least recently used frame survived")
	}
	if _, ok := c.get("a"); !ok {
		t.Error("the most recently used frame was evicted")
	}
	if n, used := c.stats(); used > 300 || n != 3 {
		t.Errorf("cache holds %d frames and %d bytes, over its 300 byte ceiling", n, used)
	}

	// Something larger than the whole cache is not worth evicting everything
	// for, and an error answer is never cached at all.
	put("huge", 400)
	if _, ok := c.get("huge"); ok {
		t.Error("a frame bigger than the cache was stored anyway")
	}
	c.put("bad", renderedFrame{Body: []byte("x"), Cache: false})
	if _, ok := c.get("bad"); ok {
		t.Error("an uncacheable answer was cached")
	}
}

// TestRenderOnceCollapsesDuplicates: a scrubber fires a request per input
// event, and several of them land on the same frame. They must cost one
// render between them, not one each.
func TestRenderOnceCollapsesDuplicates(t *testing.T) {
	var runs int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = renderOnce("same-frame", func() (renderedFrame, error) {
				atomic.AddInt32(&runs, 1)
				<-release // hold the render open so the others pile up behind it
				return renderedFrame{Body: []byte("frame"), Cache: true}, nil
			})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Errorf("%d renders for one frame, want 1", got)
	}
	if _, ok := frames.get("same-frame"); !ok {
		t.Error("the render was not cached for the next request")
	}
}

// TestRenderQueueIsBounded — the ceiling on concurrent renders is the whole
// reason a drag no longer forks forty ffmpegs.
func TestRenderQueueIsBounded(t *testing.T) {
	if cap(renderSem) != maxFrameRenders {
		t.Fatalf("queue holds %d, maxFrameRenders is %d", cap(renderSem), maxFrameRenders)
	}
	if maxFrameRenders < 2 || maxFrameRenders > 4 {
		t.Errorf("maxFrameRenders is %d; it is meant to stay between 2 and 4", maxFrameRenders)
	}

	var live, peak int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = renderOnce(fmt.Sprintf("bounded-%d", n), func() (renderedFrame, error) {
				cur := atomic.AddInt32(&live, 1)
				for {
					old := atomic.LoadInt32(&peak)
					if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
				atomic.AddInt32(&live, -1)
				return renderedFrame{Body: []byte("x"), Cache: true}, nil
			})
		}(i)
	}
	wg.Wait()
	if int(peak) > maxFrameRenders {
		t.Errorf("%d renders ran at once, ceiling is %d", peak, maxFrameRenders)
	}
}

// TestFrameKeyCoversEverythingThatChangesThePicture — a cache key that misses
// a parameter serves the wrong picture, which is worse than serving none.
func TestFrameKeyCoversEverythingThatChangesThePicture(t *testing.T) {
	base := frameRequest{Clip: "Shot_010", View: "comp", Index: 4, BG: "checker",
		Despill: 5, Source: "/clips/Shot_010/Output/Matte/00004.exr", Stamp: "a-b"}
	seen := map[string]string{base.key(): "base"}
	for name, alt := range map[string]frameRequest{
		"clip":    {Clip: "Shot_020", View: "comp", Index: 4, BG: "checker", Despill: 5, Source: base.Source, Stamp: "a-b"},
		"view":    {Clip: "Shot_010", View: "matte", Index: 4, BG: "checker", Despill: 5, Source: base.Source, Stamp: "a-b"},
		"index":   {Clip: "Shot_010", View: "comp", Index: 5, BG: "checker", Despill: 5, Source: base.Source, Stamp: "a-b"},
		"bg":      {Clip: "Shot_010", View: "comp", Index: 4, BG: "green", Despill: 5, Source: base.Source, Stamp: "a-b"},
		"despill": {Clip: "Shot_010", View: "comp", Index: 4, BG: "checker", Despill: 6, Source: base.Source, Stamp: "a-b"},
		"stamp":   {Clip: "Shot_010", View: "comp", Index: 4, BG: "checker", Despill: 5, Source: base.Source, Stamp: "a-c"},
	} {
		k := alt.key()
		if other, clash := seen[k]; clash {
			t.Errorf("changing the %s gives the same key as %s", name, other)
		}
		seen[k] = name
	}
	if base.etag() != `"`+base.key()+`"` {
		t.Errorf("the etag is not the quoted key: %s", base.etag())
	}
}

// TestStoredTokenSurvivesARestart is the bug this test exists for: -token auto
// used to mint a new secret every start, so the link in the console, the one
// in a bookmark and the one a login banner printed all went stale at once and
// the UI answered "that token was not accepted".
func TestStoredTokenSurvivesARestart(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.ClipsDir = filepath.Join(t.TempDir(), "ClipsForInference")

	first := storedToken()
	if len(first) != 24 {
		t.Fatalf("token %q is not 12 bytes of hex", first)
	}
	if got := storedToken(); got != first {
		t.Errorf("a second start minted a new token: %q then %q", first, got)
	}

	b, err := os.ReadFile(tokenPath())
	if err != nil {
		t.Fatalf("the token was not written: %v", err)
	}
	if strings.TrimSpace(string(b)) != first {
		t.Errorf("the file holds %q, the server uses %q", strings.TrimSpace(string(b)), first)
	}
	if st, err := os.Stat(tokenPath()); err == nil && st.Mode().Perm() != 0o600 {
		t.Errorf("the token file is mode %v, want 0600", st.Mode().Perm())
	}

	// It lives beside the clips, which is the volume on a rented box. In the
	// image, /opt/corridorkey is a layer that a container replacement throws
	// away, so the store must not land under the root.
	if dir := filepath.Dir(tokenPath()); dir != filepath.Dir(cfg.ClipsDir) {
		t.Errorf("the token store is at %s, not beside the clips", dir)
	}
}

// TestWhitespaceInAStoredTokenIsIgnored — the file is something an operator
// may well write by hand or echo into place.
func TestWhitespaceInAStoredTokenIsIgnored(t *testing.T) {
	old := cfg
	defer func() { cfg = old }()
	cfg.ClipsDir = filepath.Join(t.TempDir(), "ClipsForInference")
	if err := os.WriteFile(tokenPath(), []byte("  abc123\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := storedToken(); got != "abc123" {
		t.Errorf("storedToken() = %q, want %q", got, "abc123")
	}
}
