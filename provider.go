package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
)

// A Provider is whatever actually does the keying. The UI, the queue and the
// clip layout on disk are the same for all of them; a provider only has to
// turn a clip folder into AlphaHint/ and Output/.
//
// Two ship in the box:
//
//   - "corridorkey2": the real thing. Runs this repo's own infer.py, so it
//     needs the checkout, its venv and the downloaded weights. None of that
//     is in the image.
//   - "mock": a plain green-screen key in Go. No model, no CUDA, no Python.
//     It exists so the whole UI can be driven on a laptop before anyone rents
//     a GPU, and it is what a dev run selects.
type Provider interface {
	ID() string
	Title() string
	// Ready reports whether this provider can run here, and if not, why.
	Ready() (bool, string)
	// NeedsModules lists module keys this provider cannot work without.
	NeedsModules() []string
	// Run executes one action against one clip (or every ready clip, when
	// clip is empty), writing progress lines to log.
	Run(ctx context.Context, action, clip string, p Params, log func(string)) error
}

var providers = map[string]Provider{}

func registerProvider(p Provider) { providers[p.ID()] = p }

// The provider the queue hands jobs to. It is not fixed for the life of the
// process: a fresh box starts on the mock because nothing is downloaded yet,
// and has to flip to the real keyer the moment provisioning finishes. Reads
// come from every HTTP handler, so it is guarded.
var (
	providerMu     sync.RWMutex
	current        Provider
	providerChoice = "auto" // what -provider asked for
)

func activeProvider() Provider {
	providerMu.RLock()
	defer providerMu.RUnlock()
	return current
}

func setProvider(p Provider) {
	providerMu.Lock()
	current = p
	providerMu.Unlock()
}

// refreshProvider re-runs an "auto" choice after something that could change
// the answer — a finished download, a built venv. Without it a box that has
// just provisioned itself keeps serving the mock keyer until someone
// restarts the container. An explicit -provider is left alone.
func refreshProvider() {
	if providerChoice != "" && providerChoice != "auto" {
		return
	}
	p, why := pickProvider("auto")
	if p == nil {
		return
	}
	if prev := activeProvider(); prev != nil && prev.ID() == p.ID() {
		return
	}
	setProvider(p)
	log.Printf("ck-web: provider is now %s — %s", p.ID(), why)
	broadcast(event{Kind: "modules"})
}

// pickProvider resolves the -provider flag. "auto" prefers the real keyer and
// falls back to the mock, so a box with nothing installed still serves a
// working UI instead of an error page.
func pickProvider(want string) (Provider, string) {
	switch want {
	case "", "auto":
		// The real keyer if it can run here, the mock otherwise, so a box
		// with nothing downloaded still serves a working UI.
		var why string
		for _, id := range []string{"corridorkey2"} {
			p := providers[id]
			if p == nil {
				continue
			}
			ok, detail := p.Ready()
			if ok {
				return p, "auto: " + p.Title() + " is installed"
			}
			if why == "" {
				why = detail
			}
		}
		if why == "" {
			why = "no real keyer is registered"
		}
		return providers["mock"], "auto: falling back to mock — " + why
	default:
		p := providers[want]
		if p == nil {
			return nil, fmt.Sprintf("no provider named %q", want)
		}
		return p, "selected explicitly"
	}
}

func providerInfo() []map[string]any {
	out := []map[string]any{}
	for _, id := range []string{"corridorkey2", "mock"} {
		p := providers[id]
		if p == nil {
			continue
		}
		ok, why := p.Ready()
		out = append(out, map[string]any{
			"id": p.ID(), "title": p.Title(), "ready": ok, "detail": why,
			"needs":  p.NeedsModules(),
			"active": activeProvider() != nil && activeProvider().ID() == p.ID(),
		})
	}
	return out
}

// runLogged starts cmd with both streams on one pipe, so the log keeps the
// order the process printed in, and forwards every line.
func runLogged(cmd *exec.Cmd, log func(string)) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return err
	}
	pw.Close() // the child holds the only remaining writer
	scanLines(pr, log)
	pr.Close()
	return cmd.Wait()
}
