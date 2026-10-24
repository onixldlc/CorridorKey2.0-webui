package main

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"sync"
)

// event is one server-sent push: a log line, a job state change, or a hint
// that the clip list moved.
type event struct {
	Kind  string `json:"kind"` // log | job | progress | clips | modules
	JobID int    `json:"jobId,omitempty"`
	Line  string `json:"line,omitempty"`
	// Prog rides along with a progress event so the page can move one bar
	// without re-fetching the queue once a second.
	Prog *Progress `json:"prog,omitempty"`
}

var hub = struct {
	sync.Mutex
	subs map[chan event]struct{}
}{subs: map[chan event]struct{}{}}

func broadcast(e event) {
	hub.Lock()
	for ch := range hub.subs {
		select {
		case ch <- e:
		default: // a slow reader loses lines rather than stalling the job
		}
	}
	hub.Unlock()
}

// subscribers is how many pages are currently watching the stream. The
// console says so on connect and disconnect: "nothing is happening" reads
// very differently when nobody is attached to see it.
func subscribers() int {
	hub.Lock()
	defer hub.Unlock()
	return len(hub.subs)
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan event, 256)
	hub.Lock()
	hub.subs[ch] = struct{}{}
	hub.Unlock()
	defer func() {
		hub.Lock()
		delete(hub.subs, ch)
		hub.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if err := enc.Encode(e); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// gpuInfo asks nvidia-smi what is in the box. An empty slice means no GPU
// was found, which is worth showing: CPU keying is very slow.
func gpuInfo() []map[string]string {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=name,memory.total,memory.used,utilization.gpu",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	var gpus []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ", ")
		if len(f) < 4 {
			continue
		}
		gpus = append(gpus, map[string]string{
			"name": f[0], "memTotal": f[1], "memUsed": f[2], "util": f[3],
		})
	}
	return gpus
}
