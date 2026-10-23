package main

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// A Module is a downloadable piece of the keyer — a checkpoint, or a whole
// weights repo. None of them are in the image: between them they are tens of
// gigabytes, and most runs need one. They are fetched at runtime into the
// volume mounted at -root, which is why the image stays small and a dev run
// needs no network at all.
//
// The sources below are the ones scripts/setup_models.py uses, so a checkout
// populated by that installer and one populated here look the same on disk.
type Module struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Note     string `json:"note"`
	Source   string `json:"source"` // the human-readable page
	Size     string `json:"size"`
	Required bool   `json:"required"`

	// Need is what this module costs on disk: the sum of the published file
	// sizes at the pinned revision, less whatever Exclude drops. It is only
	// used to decide whether the box can provision itself without filling
	// the volume, so it is an estimate, not a contract.
	Need int64  `json:"need"`
	Dest string `json:"dest"` // relative to -root

	// Exactly one of these describes what to fetch.
	Repo  string   `json:"repo,omitempty"`  // a whole Hugging Face model repo
	Files []string `json:"files,omitempty"` // paths inside Repo, or absolute URLs

	// Rev pins a Hugging Face revision. The foundation models are pinned
	// upstream in scripts/download_weights.py and the runtime expects those
	// exact tensors, so "whatever main points at today" is not good enough.
	Rev string `json:"rev,omitempty"`

	// Exclude drops files from a whole-repo fetch by suffix. C-RADIO ships a
	// half-precision .pth.tar that duplicates model.safetensors and is never
	// read; without this every install wastes 1.2 GB.
	Exclude []string `json:"exclude,omitempty"`

	// MD5B64 is a base64 MD5 for a single-file module, checked after the
	// download. Upstream publishes one for the RVM checkpoint, which is the
	// only weight that does not come from Hugging Face.
	MD5B64 string `json:"-"`

	// Marker is the path, under Dest, whose presence means "installed".
	Marker string `json:"marker"`
}

var modules = []Module{
	{
		Key: "corridorkey2", Title: "CorridorKey 2.0",
		Note:   "The keyer. One model for both green and blue — there is no separate blue checkpoint any more.",
		Source: "https://huggingface.co/nikopueringer/CorridorKey2.0",
		Size:   "2.66 GB", Required: true, Need: 2539 * mib,
		Dest:   "weights",
		Repo:   "nikopueringer/CorridorKey2.0",
		Files:  []string{"corridorkey_v2.safetensors", "foundation_stats.safetensors"},
		Marker: "corridorkey_v2.safetensors",
	},
	{
		Key: "cradio", Title: "C-RADIOv4-SO400M",
		Note:   "Frozen vision backbone from NVIDIA. Runs on every pass; not optional.",
		Source: "https://huggingface.co/nvidia/C-RADIOv4-SO400M",
		Size:   "1.73 GB", Required: true, Need: 1646 * mib,
		Dest:    "vfms/cradio/C-RADIOv4-SO400M",
		Repo:    "nvidia/C-RADIOv4-SO400M",
		Rev:     "c0457f5dc26ca145f954cd4fc5bb6114e5705ad8",
		Exclude: []string{".pth.tar"},
		Marker:  "model.safetensors",
	},
	{
		Key: "moge", Title: "MoGe-2 ViT-B",
		Note:   "Frozen geometry model from Microsoft. Runs on every pass; not optional.",
		Source: "https://huggingface.co/Ruicheng/moge-2-vitb-normal",
		Size:   "419 MB", Required: true, Need: 400 * mib,
		Dest:   "vfms/moge",
		Repo:   "Ruicheng/moge-2-vitb-normal",
		Rev:    "ca5f0e07ff01d3e5a364c1d954ed12ee1814b368",
		Files:  []string{"model.pt"},
		Marker: "model.pt",
	},
	{
		Key: "rvm", Title: "RVM small",
		Note:   "Frozen temporal model from Google DeepMind. The only weight not hosted on Hugging Face.",
		Source: "https://github.com/google-deepmind/representations4d",
		Size:   "270 MB", Required: true, Need: 258 * mib,
		Dest: "vfms/rvm",
		Files: []string{
			"https://storage.googleapis.com/representations4d/checkpoints/pretrain_rvm_small16_256_204031069.npz",
		},
		MD5B64: "ZJb6kj8+tasiJbVeplXu7Q==",
		Marker: "pretrain_rvm_small16_256_204031069.npz",
	},
}

// ModuleState is a module plus what is on this box right now.
type ModuleState struct {
	Module
	Present    bool   `json:"present"`
	Bytes      int64  `json:"bytes"`
	Path       string `json:"path"`
	Installing bool   `json:"installing"`
}

func moduleByKey(key string) *Module {
	for i := range modules {
		if modules[i].Key == key {
			return &modules[i]
		}
	}
	return nil
}

func (m Module) state() ModuleState {
	full := filepath.Join(cfg.Root, m.Dest, m.Marker)
	st := ModuleState{Module: m, Path: filepath.Join(m.Dest, m.Marker)}
	if fi, err := os.Stat(full); err == nil {
		st.Present = true
		st.Bytes = fi.Size()
		if m.Repo != "" && len(m.Files) == 0 {
			// A whole-repo module: report the tree, not one file.
			st.Bytes = dirSize(filepath.Join(cfg.Root, m.Dest))
		}
	}
	st.Installing = queue.isInstalling(m.Key)
	return st
}

func moduleStates() []ModuleState {
	out := make([]ModuleState, 0, len(modules))
	for _, m := range modules {
		out = append(out, m.state())
	}
	return out
}

func dirSize(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// ── installing ──────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 0} // weights take as long as they take

// install fetches one module into the checkout. It is deliberately the only
// thing in ck-web that reaches the network.
func install(ctx context.Context, key string, log func(string)) error {
	if cfg.Dev {
		return fmt.Errorf("this is a dev run (-mode dev): downloads are off, and the mock provider needs none")
	}
	m := moduleByKey(key)
	if m == nil {
		return fmt.Errorf("no module named %q", key)
	}
	dest := filepath.Join(cfg.Root, m.Dest)
	if err := os.MkdirAll(dest, 0o775); err != nil {
		return err
	}

	type item struct{ url, rel string }
	var items []item

	switch {
	case len(m.Files) > 0:
		for _, f := range m.Files {
			if strings.HasPrefix(f, "http") {
				items = append(items, item{f, path.Base(f)})
				continue
			}
			items = append(items, item{hfURL(m.Repo, m.Rev, f), f})
		}
	case m.Repo != "":
		log("Listing " + m.Repo)
		files, err := hfTree(ctx, m.Repo, m.Rev)
		if err != nil {
			return err
		}
		for _, f := range files {
			if m.skip(f) {
				log("skipping " + f + " — not read by the runtime")
				continue
			}
			items = append(items, item{hfURL(m.Repo, m.Rev, f), f})
		}
		log(fmt.Sprintf("%d files to fetch", len(items)))
	default:
		return fmt.Errorf("module %q has no source", key)
	}

	for i, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		out := filepath.Join(dest, filepath.FromSlash(it.rel))
		if !strings.HasPrefix(out, dest+string(os.PathSeparator)) && out != filepath.Join(dest, it.rel) {
			return fmt.Errorf("refusing a path outside %s: %s", dest, it.rel)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o775); err != nil {
			return err
		}
		if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
			log(fmt.Sprintf("[%d/%d] %s — already here", i+1, len(items), it.rel))
			continue
		}
		log(fmt.Sprintf("[%d/%d] %s", i+1, len(items), it.rel))
		if err := download(ctx, it.url, out, log); err != nil {
			return fmt.Errorf("%s: %w", it.rel, err)
		}
		if m.MD5B64 != "" {
			sum, err := md5Base64(out)
			if err != nil {
				return err
			}
			if sum != m.MD5B64 {
				os.Remove(out)
				return fmt.Errorf("%s: checksum mismatch (got %s, want %s) — the file was removed",
					it.rel, sum, m.MD5B64)
			}
			log("    checksum ok")
		}
	}

	st := m.state()
	if !st.Present {
		return fmt.Errorf("download finished but %s is missing — the source layout may have changed", st.Path)
	}
	log(fmt.Sprintf("%s installed (%s on disk)", m.Title, humanBytes(st.Bytes)))
	broadcast(event{Kind: "modules"})
	return nil
}

// skip reports whether a whole-repo fetch should leave this file alone.
func (m Module) skip(file string) bool {
	for _, suffix := range m.Exclude {
		if strings.HasSuffix(file, suffix) {
			return true
		}
	}
	return false
}

// md5Base64 is the digest form upstream publishes for the RVM checkpoint.
func md5Base64(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

func hfURL(repo, rev, file string) string {
	if rev == "" {
		rev = "main"
	}
	return "https://huggingface.co/" + repo + "/resolve/" + rev + "/" + file + "?download=true"
}

// hfTree lists a Hugging Face repo's files. Only the model tree API is used;
// no token, so it works for public repos only.
func hfTree(ctx context.Context, repo, rev string) ([]string, error) {
	if rev == "" {
		rev = "main"
	}
	u := "https://huggingface.co/api/models/" + repo + "/tree/" + rev + "?recursive=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing %s: %s", repo, res.Status)
	}
	var entries []struct {
		Type string `json:"type"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(res.Body).Decode(&entries); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		if strings.HasPrefix(e.Path, ".") { // .gitattributes and friends
			continue
		}
		out = append(out, e.Path)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no files", repo)
	}
	return out, nil
}

// download fetches one file to disk via a .part file, reporting progress at
// most once a second so a 37 GB fetch does not flood the console. It prefers
// aria2c, which opens several connections to the same object; the body below
// is the one-stream fallback for an image without it.
func download(ctx context.Context, url, out string, log func(string)) error {
	if _, ok := aria2Available(); ok {
		return aria2Download(ctx, url, out, log)
	}
	return downloadStream(ctx, url, out, log)
}

func downloadStream(ctx context.Context, url, out string, log func(string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", res.Status)
	}

	part := out + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	pr := &progress{total: res.ContentLength, log: log, last: time.Now(), name: filepath.Base(out)}
	_, copyErr := io.Copy(io.MultiWriter(f, pr), res.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(part)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return os.Rename(part, out)
}

type progress struct {
	total int64
	done  int64
	name  string
	last  time.Time
	log   func(string)
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) < time.Second {
		return len(b), nil
	}
	p.last = time.Now()
	if p.total > 0 {
		p.log(fmt.Sprintf("    %s — %s / %s (%d%%)", p.name,
			humanBytes(p.done), humanBytes(p.total), p.done*100/p.total))
	} else {
		p.log(fmt.Sprintf("    %s — %s", p.name, humanBytes(p.done)))
	}
	return len(b), nil
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.2f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KB", float64(n)/k)
	}
	return fmt.Sprintf("%d B", n)
}

// uninstall removes a module's files. Only the module's own destination is
// touched, and never a directory shared with another module.
func uninstall(key string) error {
	m := moduleByKey(key)
	if m == nil {
		return fmt.Errorf("no module named %q", key)
	}
	shared := 0
	for _, o := range modules {
		if o.Dest == m.Dest {
			shared++
		}
	}
	if shared > 1 || len(m.Files) > 0 {
		// Several modules land here, so remove the marker file only.
		return os.Remove(filepath.Join(cfg.Root, m.Dest, m.Marker))
	}
	return os.RemoveAll(filepath.Join(cfg.Root, m.Dest))
}
