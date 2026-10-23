package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	w, h, frames := parseProbe("width=3840\nheight=2160\navg_frame_rate=25/1\n" +
		"duration=2.000000\nnb_frames=50\n")
	if w != 3840 || h != 2160 || frames != 50 {
		t.Errorf("mp4 read as %dx%d, %d frames", w, h, frames)
	}

	w, h, frames = parseProbe("width=1920\nheight=1080\navg_frame_rate=24/1\n" +
		"duration=N/A\nnb_frames=N/A\n")
	if w != 1920 || h != 1080 {
		t.Errorf("mkv read as %dx%d", w, h)
	}
	if frames != 0 {
		t.Errorf("frame count is %d, want 0 — unknown, not invented", frames)
	}
	// Unknown length must still plan, as a clip longer than any window.
	if p := planVRAM(w, h, frames, 23.3); !p.Fits || p.Frames != 80 {
		t.Errorf("a clip of unknown length planned as %+v", p)
	}
}
