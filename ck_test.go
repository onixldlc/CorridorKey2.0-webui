package main

import (
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
