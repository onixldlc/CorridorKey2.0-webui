package main

// Nobody rents a GPU box to click four Install buttons. If this box looks like
// a production box — a CUDA GPU, and enough free disk to hold what is missing —
// ck-web provisions itself the moment it starts: the inference stack into the
// venv first, then every required weight, all on the net lane so the GPU is
// never held up by a download.
//
// The two gates are exactly the two conditions that matter. Without a GPU
// infer.py refuses to run at all, so fetching ten gigabytes would be waste.
// Without the disk to finish, the download dies somewhere in the middle and
// leaves a half-populated volume, which is worse than never starting. In
// either case nothing is lost: the Modules page still installs by hand, and
// POST /api/provision re-runs the decision once the operator has fixed
// whatever was in the way.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
)

const mib = 1 << 20

// What the missing pieces cost on disk.
//
// The weight figures live on the modules themselves. These two are the stack:
// aria2c pulls about 3.1 GiB of wheels into /data/wheels and keeps them there
// so an image upgrade does not re-download them (CKWEB_KEEP_WHEELS=0 purges
// them after the install), and pip unpacks roughly two and a half times that
// into the venv.
const (
	stackWheelsBytes    = 3163 * mib
	stackInstalledBytes = 7600 * mib

	// Room for a plate and the four EXR stages a key writes beside it, so
	// provisioning does not hand back a volume with nowhere to work.
	provisionHeadroom = 8192 * mib
)

// DiskNeed is one filesystem and what provisioning wants from it.
type DiskNeed struct {
	What string `json:"what"`
	Path string `json:"path"`
	Need int64  `json:"need"`
	Free int64  `json:"free"` // -1 when statfs could not answer
}

// Provision is the decision and everything it was based on, so the UI can
// explain a "no" instead of just showing four missing rows.
type Provision struct {
	Mode    string     `json:"mode"`
	Auto    bool       `json:"auto"`   // did ck-web start the downloads itself
	Reason  string     `json:"reason"` // why, either way
	GPUs    int        `json:"gpus"`
	Stack   bool       `json:"stack"` // is the venv already built
	Missing []string   `json:"missing"`
	Disks   []DiskNeed `json:"disks"`
	Jobs    []int      `json:"jobs,omitempty"`
}

var (
	provisionMu sync.Mutex
	provisioned Provision // the last decision, for /api/health
)

// planProvision measures the box. It reads only; the decision and the jobs
// come after.
func planProvision() Provision {
	p := Provision{Mode: cfg.Mode, GPUs: len(gpuInfo()), Stack: stackPresent(), Missing: []string{}}

	var weights int64
	dest := cfg.Root
	for _, m := range modules {
		if !m.Required || m.state().Present {
			continue
		}
		p.Missing = append(p.Missing, m.Key)
		weights += m.Need
		if len(p.Missing) == 1 {
			dest = filepath.Join(cfg.Root, m.Dest)
		}
	}

	var want []DiskNeed
	if !p.Stack {
		want = append(want, DiskNeed{
			What: "inference stack",
			Path: venvDir(),
			Need: stackWheelsBytes + stackInstalledBytes,
		})
	}
	if weights > 0 {
		want = append(want, DiskNeed{What: "weights", Path: dest, Need: weights})
	}
	if len(want) > 0 {
		want = append(want, DiskNeed{
			What: "clips and their output",
			Path: cfg.ClipsDir,
			Need: provisionHeadroom,
		})
	}
	p.Disks = foldNeeds(want)
	return p
}

// foldNeeds folds the targets onto the filesystems that actually hold them. In
// the image /data holds all three, so the figures have to be summed before
// they are compared with one free-space number — three separate checks would
// each pass against the same spare gigabyte.
func foldNeeds(in []DiskNeed) []DiskNeed {
	out := []DiskNeed{}
	seen := map[string]int{}
	for _, n := range in {
		id, free := fsOf(n.Path)
		if i, ok := seen[id]; ok && id != "" {
			out[i].What += ", " + n.What
			out[i].Need += n.Need
			continue
		}
		n.Free = free
		seen[id] = len(out)
		out = append(out, n)
	}
	return out
}

// fsOf identifies the filesystem a path will land on and how much of it is
// free, walking up to the first parent that exists: on a freshly mounted
// volume the leaf directory is not there yet.
func fsOf(path string) (string, int64) {
	var st syscall.Statfs_t
	for path != "" {
		if err := syscall.Statfs(path, &st); err == nil {
			// Bavail, not Bfree: the reserved blocks are not ours.
			return fmt.Sprint(st.Fsid), int64(st.Bavail) * int64(st.Bsize)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", -1
		}
		path = parent
	}
	return "", -1
}

// provisionDecision is the gate. It is pure so the rules can be tested without
// a GPU, a volume or a network.
func provisionDecision(p Provision) (bool, string) {
	switch {
	case p.Mode == "dev":
		return false, "dev run: downloads are off, and the mock keyer needs none"
	case p.Mode == "manual":
		return false, "-mode manual: nothing is fetched until the Modules page asks"
	case p.Stack && len(p.Missing) == 0:
		return false, "already provisioned: the inference stack and every weight are on disk"
	case p.GPUs == 0 && p.Mode != "prod":
		return false, "no CUDA GPU — nvidia-smi reported none, and infer.py refuses a CPU-only box. " +
			"Run with -mode prod to download anyway"
	}
	for _, d := range p.Disks {
		if d.Free < 0 {
			return false, "could not read the free space on " + d.Path
		}
		if d.Free < d.Need {
			return false, fmt.Sprintf("not enough disk: %s needs %s for the %s and has %s",
				d.Path, humanBytes(d.Need), d.What, humanBytes(d.Free))
		}
	}
	var total int64
	for _, d := range p.Disks {
		total += d.Need
	}
	what := fmt.Sprintf("%d weight(s)", len(p.Missing))
	if !p.Stack {
		what = "the inference stack and " + what
	}
	return true, fmt.Sprintf("%d GPU(s) and room for %s: fetching %s",
		p.GPUs, humanBytes(total), what)
}

// autoProvision measures the box, decides, and queues the work. It is called
// once at startup and again by POST /api/provision, which is how an operator
// retries after freeing disk. Jobs already queued are not queued twice.
func autoProvision() Provision {
	p := planProvision()
	p.Auto, p.Reason = provisionDecision(p)

	if p.Auto {
		if !p.Stack && !queue.isBootstrapping() {
			j := queue.SubmitBootstrap()
			p.Jobs = append(p.Jobs, j.ID)
		}
		for _, key := range p.Missing {
			if queue.isInstalling(key) {
				continue
			}
			j := queue.SubmitInstall(key)
			p.Jobs = append(p.Jobs, j.ID)
		}
	}

	provisionMu.Lock()
	provisioned = p
	provisionMu.Unlock()
	broadcast(event{Kind: "modules"})
	return p
}

// provisionStatus re-measures the box but keeps the decision that was made,
// so a page loaded mid-download shows both what was decided and where it is.
func provisionStatus() Provision {
	p := planProvision()
	provisionMu.Lock()
	p.Auto, p.Reason, p.Jobs = provisioned.Auto, provisioned.Reason, provisioned.Jobs
	provisionMu.Unlock()
	if p.Reason == "" {
		_, p.Reason = provisionDecision(p)
	}
	return p
}

// logProvision prints the decision beside the rest of the startup banner,
// because on a rented box the console scrollback is the only log there is.
func logProvision(p Provision) {
	verb := "manual"
	if p.Auto {
		verb = "auto"
	}
	log.Printf("  setup    %s — %s", verb, p.Reason)
	for _, d := range p.Disks {
		log.Printf("           %s: %s needed for %s, %s free",
			d.Path, humanBytes(d.Need), d.What, humanBytes(d.Free))
	}
	if len(p.Jobs) > 0 {
		log.Printf("           queued jobs %v — watch them on the Queue page", p.Jobs)
	}
}

// ── the inference stack ─────────────────────────────────────────────────────

// stackPresent reports whether the venv has already been built. It tests the
// same stamp bootstrap.sh writes, so the two agree on what "done" means.
func stackPresent() bool {
	if _, err := os.Stat(cfg.Python); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(venvDir(), ".ck2-stack")); err != nil {
		return false
	}
	return true
}

// venvDir is where the stack lives. CKWEB_VENV is what the image sets and what
// bootstrap.sh reads; otherwise it is inferred from the interpreter path.
func venvDir() string {
	if v := os.Getenv("CKWEB_VENV"); v != "" {
		return v
	}
	if dir := filepath.Dir(filepath.Dir(cfg.Python)); dir != "." && dir != string(os.PathSeparator) {
		return dir // <venv>/bin/python
	}
	// -python is a bare name found on PATH, so it names no venv. Report where
	// the stack would be built, which is what the image sets CKWEB_VENV to.
	return filepath.Join(cfg.Root, "venv")
}

// runBootstrap shells out to the script in the image that fetches the wheels
// with aria2c and builds the venv from them. It stays a shell script rather
// than Go because it is also what a console session runs by hand, and because
// pip is not worth reimplementing here.
func runBootstrap(ctx context.Context, log func(string)) error {
	if cfg.Dev {
		return fmt.Errorf("this is a dev run (-mode dev): the mock keyer needs no torch")
	}
	if stackPresent() {
		log("the inference stack is already in " + venvDir() + " — nothing to download")
		return nil
	}
	if _, err := os.Stat(cfg.Bootstrap); err != nil {
		return fmt.Errorf("no bootstrap script at %s — run the image, or build the venv by hand "+
			"and point -python at it", cfg.Bootstrap)
	}
	cmd := exec.CommandContext(ctx, cfg.Bootstrap)
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	log("$ " + cfg.Bootstrap)
	if err := runLogged(cmd, log); err != nil {
		return err
	}
	if !stackPresent() {
		return fmt.Errorf("%s exited clean but %s/.ck2-stack is not there — the stack is not usable",
			cfg.Bootstrap, venvDir())
	}
	log("inference stack ready in " + venvDir())
	return nil
}
