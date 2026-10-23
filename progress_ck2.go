package main

// How far along a key is.
//
// infer.py prints one line per Hann window and nothing at all during the two
// prepasses, which at 4K is twenty minutes of a blank log. The prepasses do
// leave a trail though: both write their features into numpy memmaps, created
// at full size and filled front to back, so the blocks the filesystem has
// actually allocated under the file are an exact measure of how far they have
// got. That is why --temp_dir is pointed at the clip rather than left in /tmp:
// so this can watch it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Progress is one bar in the queue: a phase, where it is inside that phase,
// and which phase of how many this is.
type Progress struct {
	Phase string `json:"phase"`
	Step  int    `json:"step"`
	Steps int    `json:"steps"`
	Pct   int    `json:"pct"` // -1 when the phase cannot be measured
	Note  string `json:"note,omitempty"`
}

// The four phases of a key, in the order infer.py runs them.
const (
	ckPhaseLoad   = 1
	ckPhaseRVM    = 2
	ckPhaseVFM    = 3
	ckPhaseKey    = 4
	ckPhaseCount  = 4
	ckPollEvery   = 2 * time.Second
	ckSendAtLeast = 900 * time.Millisecond
)

type ck2Tracker struct {
	job *Job
	dir string // --temp_dir, where the prepass memmaps land

	mu      sync.Mutex
	step    int
	phase   string
	pct     int
	note    string
	frames  int
	window  int
	windows int
	begun   time.Time
	sent    time.Time
}

func newCK2Tracker(j *Job, tempDir string) *ck2Tracker {
	t := &ck2Tracker{job: j, dir: tempDir, step: ckPhaseLoad,
		phase: "loading the model", pct: -1, begun: time.Now()}
	t.publish(true)
	return t
}

// ck2TempDir is where infer.py is told to put its scratch. Both the provider
// and the tracker derive it from the clip name, so neither has to tell the
// other where it is.
func ck2TempDir(clip string) string {
	return filepath.Join(cfg.ClipsDir, clip, "Output", ".v2tmp")
}

var (
	reProbed  = regexp.MustCompile(`probed (\d+) frames`)
	reWindows = regexp.MustCompile(`windows=(\d+)`)
	reWindow  = regexp.MustCompile(`window (\d+)/(\d+) \[(\d+):(\d+)\]`)
)

// line reads one line of infer.py's output for the phase it announces. It is
// called for every line; the ones it does not recognise cost a few compares.
func (t *ck2Tracker) line(s string) {
	t.mu.Lock()
	changed := false
	if m := reProbed.FindStringSubmatch(s); m != nil {
		t.frames, _ = strconv.Atoi(m[1])
	}
	switch {
	case strings.Contains(s, "RVM continuous prepass"):
		t.enter(ckPhaseRVM, "RVM prepass")
		changed = true
	case strings.Contains(s, "frozen VFM prepass"):
		t.enter(ckPhaseVFM, "foundation prepass")
		changed = true
	case strings.Contains(s, "Hann stage1:"):
		t.enter(ckPhaseKey, "keying")
		if m := reWindows.FindStringSubmatch(s); m != nil {
			t.windows, _ = strconv.Atoi(m[1])
		}
		changed = true
	}
	if m := reWindow.FindStringSubmatch(s); m != nil {
		t.window, _ = strconv.Atoi(m[1])
		t.windows, _ = strconv.Atoi(m[2])
		done, _ := strconv.Atoi(m[4])
		if t.windows > 0 {
			t.pct = t.window * 100 / t.windows
		}
		t.note = fmt.Sprintf("window %d of %d · frame %d", t.window, t.windows, done)
		if t.frames > 0 {
			t.note = fmt.Sprintf("window %d of %d · frame %d of %d",
				t.window, t.windows, done, t.frames)
		}
		changed = true
	}
	t.mu.Unlock()
	t.publish(changed)
}

// enter switches phase. Caller holds the lock.
func (t *ck2Tracker) enter(step int, name string) {
	if t.step == step {
		return
	}
	t.step, t.phase, t.pct, t.note, t.begun = step, name, 0, "", time.Now()
}

// watch fills in the part no line reports: how far the two prepasses have
// written into their memmaps. It stops with the job.
func (t *ck2Tracker) watch(ctx context.Context) {
	tick := time.NewTicker(ckPollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.mu.Lock()
			var pct int
			switch t.step {
			case ckPhaseRVM:
				pct = sparseFill(t.dir, "*_rvm_feat.f16")
			case ckPhaseVFM:
				pct = sparseFill(t.dir, "*_foundation_moge.f32", "*_foundation_cradio.f32")
			default:
				t.mu.Unlock()
				continue
			}
			if pct > t.pct {
				t.pct = pct
			}
			t.note = t.eta()
			t.mu.Unlock()
			t.publish(false)
		}
	}
}

// eta is a straight line through the phase so far. Caller holds the lock.
func (t *ck2Tracker) eta() string {
	if t.pct <= 0 || t.pct >= 100 {
		return ""
	}
	elapsed := time.Since(t.begun)
	left := time.Duration(float64(elapsed) * float64(100-t.pct) / float64(t.pct))
	return fmt.Sprintf("about %s left in this phase", left.Round(time.Second))
}

func (t *ck2Tracker) publish(force bool) {
	t.mu.Lock()
	if !force && time.Since(t.sent) < ckSendAtLeast {
		t.mu.Unlock()
		return
	}
	t.sent = time.Now()
	p := &Progress{Phase: t.phase, Step: t.step, Steps: ckPhaseCount, Pct: t.pct, Note: t.note}
	t.mu.Unlock()
	t.job.setProgress(p)
}

// sparseFill is how full the matching files are, as a percentage of their
// declared size, measured by the blocks the filesystem has allocated. It
// returns -1 when there is nothing to measure or the filesystem does not do
// sparse files, in which case the bar stays indeterminate rather than lying.
func sparseFill(dir string, globs ...string) int {
	var used, total int64
	for _, g := range globs {
		matches, _ := filepath.Glob(filepath.Join(dir, g))
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok || fi.Size() == 0 {
				continue
			}
			used += st.Blocks * 512
			total += fi.Size()
		}
	}
	if total == 0 {
		return -1
	}
	pct := int(used * 100 / total)
	if pct > 100 {
		pct = 100
	}
	return pct
}
