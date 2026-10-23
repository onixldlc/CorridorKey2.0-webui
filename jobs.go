package main

import (
	"bufio"
	"context"
	"io"
	"sync"
	"time"
)

// Params are the answers clip_manager.py asks for on stdin when it runs
// inference. Defaults match the CLI's own defaults, and the mock provider
// reads the same fields so the controls do something in a dev run.
type Params struct {
	Linear  bool `json:"linear"`  // input is linear light, not sRGB
	Despill int  `json:"despill"` // 0-10
	// Despeckle, SpeckSize and Refiner are the mock keyer's own knobs; the
	// 2.0 runtime has no equivalents.
	Despeckle bool    `json:"despeckle"` // auto-despeckle on
	SpeckSize int     `json:"speckSize"` // min pixels for a spot
	Refiner   float64 `json:"refiner"`   // refiner strength multiplier

	// CorridorKey 2.0.
	HintQuality float64 `json:"hintQuality"` // how far to trust a supplied hint
	LowVram     bool    `json:"lowVram"`     // staged inference for a small card
	// FitVRAM lets ck-web shrink a plate that cannot fit the card at any
	// window size, rather than failing the job. Off, such a clip is refused
	// with the resolution that would have worked.
	FitVRAM bool `json:"fitVram"`
}

// despillStrength maps the 0-10 slider onto infer.py's --despill_strength,
// which is a 0.0-1.0 fraction.
func (p Params) despillStrength() float64 {
	d := p.Despill
	if d < 0 {
		d = 0
	}
	if d > 10 {
		d = 10
	}
	return float64(d) / 10
}

func defaultParams() Params {
	return Params{Linear: false, Despill: 5, Despeckle: true, SpeckSize: 400,
		Refiner: 1.0, HintQuality: 0.95, FitVRAM: true}
}

// Lanes keep a 37 GB download from blocking the GPU, and two GPU jobs from
// overlapping. One worker each.
const (
	LaneGPU = "gpu"
	LaneNet = "net"
)

// Job is one unit of queued work: a keying action, or a module install.
type Job struct {
	ID       int       `json:"id"`
	Lane     string    `json:"lane"`
	Action   string    `json:"action"` // generate_alphas | run_inference | install
	Clip     string    `json:"clip"`   // "" means every eligible clip
	Module   string    `json:"module,omitempty"`
	Provider string    `json:"provider,omitempty"`
	State    string    `json:"state"` // queued | running | done | failed | cancelled
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	Params   Params    `json:"params"`

	// Progress is the live phase of a key: nil for anything that has none,
	// and the last phase reached once the job has stopped.
	Progress *Progress `json:"progress,omitempty"`

	log    []string
	cancel context.CancelFunc
	mu     sync.Mutex
}

func (j *Job) append(line string) {
	j.mu.Lock()
	if len(j.log) >= 4000 { // ring: keep the tail, inference is chatty
		j.log = append(j.log[:0], j.log[2000:]...)
	}
	j.log = append(j.log, line)
	j.mu.Unlock()
	broadcast(event{Kind: "log", JobID: j.ID, Line: line})
}

// setProgress replaces the live phase and tells the page about it. The
// tracker already rate-limits itself, so every call here is worth a frame.
func (j *Job) setProgress(p *Progress) {
	j.mu.Lock()
	j.Progress = p
	j.mu.Unlock()
	// Its own kind, not "job": a job event makes the page reload the queue and
	// redraw it, which at one bar update a second would throw away the console
	// scroll position the operator is reading.
	broadcast(event{Kind: "progress", JobID: j.ID, Prog: p})
}

// Tail returns at most n trailing log lines.
func (j *Job) Tail(n int) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if n <= 0 || n > len(j.log) {
		n = len(j.log)
	}
	out := make([]string, n)
	copy(out, j.log[len(j.log)-n:])
	return out
}

// Queue holds the job history and one worker per lane.
type Queue struct {
	mu     sync.Mutex
	jobs   []*Job
	lanes  map[string]chan *Job
	nextID int
	active map[string]*Job
}

var queue = &Queue{
	lanes:  map[string]chan *Job{LaneGPU: make(chan *Job, 64), LaneNet: make(chan *Job, 64)},
	active: map[string]*Job{},
	nextID: 1,
}

func (q *Queue) start() {
	for lane := range q.lanes {
		go q.worker(lane)
	}
}

func (q *Queue) worker(lane string) {
	for j := range q.lanes[lane] {
		q.mu.Lock()
		if j.State == "cancelled" {
			q.mu.Unlock()
			continue
		}
		q.active[lane] = j
		q.mu.Unlock()
		q.run(j)
		q.mu.Lock()
		delete(q.active, lane)
		q.mu.Unlock()
	}
}

// Submit queues a keying action.
func (q *Queue) Submit(action, clip string, p Params) *Job {
	j := q.add(&Job{Lane: LaneGPU, Action: action, Clip: clip, Params: p,
		Provider: activeProvider().ID()})
	q.lanes[LaneGPU] <- j
	return j
}

// SubmitBootstrap queues the one-time build of the inference stack. It shares
// the net lane with the weights, so the 3.1 GiB of wheels and the 4.7 GiB of
// checkpoints do not fight each other for the link.
func (q *Queue) SubmitBootstrap() *Job {
	j := q.add(&Job{Lane: LaneNet, Action: "bootstrap"})
	q.lanes[LaneNet] <- j
	return j
}

// SubmitInstall queues a module download.
func (q *Queue) SubmitInstall(key string) *Job {
	j := q.add(&Job{Lane: LaneNet, Action: "install", Module: key})
	q.lanes[LaneNet] <- j
	return j
}

func (q *Queue) add(j *Job) *Job {
	q.mu.Lock()
	j.ID = q.nextID
	j.State = "queued"
	q.nextID++
	q.jobs = append(q.jobs, j)
	if len(q.jobs) > 200 {
		q.jobs = q.jobs[len(q.jobs)-200:]
	}
	q.mu.Unlock()
	broadcast(event{Kind: "job", JobID: j.ID})
	return j
}

func (q *Queue) List() []*Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*Job, len(q.jobs))
	copy(out, q.jobs)
	return out
}

func (q *Queue) Get(id int) *Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// isRunning reports whether the clip is in the active GPU job. A whole-folder
// job (empty Clip) counts for every clip.
func (q *Queue) isRunning(clip string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.active[LaneGPU]
	return j != nil && (j.Clip == "" || j.Clip == clip)
}

func (q *Queue) isBootstrapping() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.Action == "bootstrap" && (j.State == "queued" || j.State == "running") {
			return true
		}
	}
	return false
}

func (q *Queue) isInstalling(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if j.Action == "install" && j.Module == key && (j.State == "queued" || j.State == "running") {
			return true
		}
	}
	return false
}

func (q *Queue) Cancel(id int) bool {
	j := q.Get(id)
	if j == nil {
		return false
	}
	j.mu.Lock()
	cancel := j.cancel
	queued := j.State == "queued"
	if queued {
		j.State = "cancelled"
	}
	j.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	if queued {
		broadcast(event{Kind: "job", JobID: j.ID})
	}
	return queued
}

// run dispatches a job to whatever performs its action and records how it
// ended. Everything a job prints goes to its own log and out over SSE.
func (q *Queue) run(j *Job) {
	ctx, cancel := context.WithCancel(context.Background())
	j.mu.Lock()
	j.cancel = cancel
	j.State = "running"
	j.Started = time.Now()
	j.mu.Unlock()
	defer cancel()
	broadcast(event{Kind: "job", JobID: j.ID})

	var err error
	switch j.Action {
	case "install":
		err = install(ctx, j.Module, j.append)
	case "bootstrap":
		err = runBootstrap(ctx, j.append)
	case "generate_alphas", "run_inference":
		p := activeProvider()
		if p == nil {
			err = errNoProvider
			break
		}
		if ok, why := p.Ready(); !ok {
			j.append("provider " + p.ID() + " is not ready: " + why)
			err = errNotReady
			break
		}
		sink := j.append
		if j.Action == "run_inference" && j.Clip != "" {
			// A key is the only job long enough to need a bar, and the only
			// one whose phases can be read out of what it prints.
			t := newCK2Tracker(j, ck2TempDir(j.Clip))
			go t.watch(ctx)
			sink = func(line string) {
				j.append(line)
				t.line(line)
			}
		}
		err = p.Run(ctx, j.Action, j.Clip, j.Params, sink)
	default:
		err = errUnknownAction
	}

	j.mu.Lock()
	j.Ended = time.Now()
	switch {
	case ctx.Err() != nil:
		j.State = "cancelled"
	case err != nil:
		j.State = "failed"
		j.Error = err.Error()
	default:
		j.State = "done"
	}
	state, jerr := j.State, j.Error
	j.cancel = nil
	j.mu.Unlock()

	if jerr != "" {
		j.append("error: " + jerr)
	}
	j.append("— " + state + " —")
	broadcast(event{Kind: "job", JobID: j.ID})
	broadcast(event{Kind: "clips"})
	if j.Action == "install" || j.Action == "bootstrap" {
		broadcast(event{Kind: "modules"})
		// The keyer may have become runnable just now, and a box that
		// provisioned itself should not need a restart to notice.
		refreshProvider()
	}
}

type jobError string

func (e jobError) Error() string { return string(e) }

const (
	errNoProvider    = jobError("no provider is active")
	errNotReady      = jobError("the active provider cannot run here")
	errUnknownAction = jobError("unknown action")
)

// scanLines forwards a stream line by line, with a buffer big enough for the
// long paths and tracebacks Python prints.
func scanLines(r io.Reader, log func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		log(sc.Text())
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
