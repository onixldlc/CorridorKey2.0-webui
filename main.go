// ck-web serves a browser UI for EZ-CorridorKey on a remote GPU box.
//
// The keyer itself stays where it lives: this process shells out to the
// repo's own clip_manager.py and only owns the HTTP surface, the upload
// folder and the job queue.
package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

// Config is everything the server needs to know about its host.
type Config struct {
	Addr      string // listen address
	Root      string // data root: clips, and where modules are installed
	ClipsDir  string // ClipsForInference
	Python    string // interpreter inside the project venv
	FFmpeg    string // ffmpeg binary, for extraction and export
	Aria2     string // aria2c binary for module downloads; "off" forces the Go path
	Bootstrap string // script that builds the inference stack in the volume
	Token     string // shared secret; empty disables auth
	MaxUpload int64  // bytes
	Mode      string // auto | dev | prod | manual
	Dev       bool   // dev run: mock provider, no downloads
	Version   string // from the VERSION file, stamped at build time
}

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

var cfg Config

func main() {
	flag.StringVar(&cfg.Addr, "addr", env("CKWEB_ADDR", ":8080"), "listen address")
	flag.StringVar(&cfg.Root, "root", env("CKWEB_ROOT", "/data"), "data root: clips, and where modules install to")
	flag.StringVar(&cfg.Mode, "mode", env("CKWEB_MODE", "auto"), "auto | dev | prod | manual — dev forces the mock provider and blocks downloads, manual turns off self-provisioning")
	providerFlag := flag.String("provider", env("CKWEB_PROVIDER", "auto"), "auto | corridorkey | mock")
	flag.StringVar(&cfg.ClipsDir, "clips", env("CKWEB_CLIPS", ""), "clips dir (default <root>/ClipsForInference)")
	flag.StringVar(&cfg.Python, "python", env("CKWEB_PYTHON", ""), "python binary (default <root>/venv/bin/python)")
	flag.StringVar(&cfg.FFmpeg, "ffmpeg", env("CKWEB_FFMPEG", "ffmpeg"), "ffmpeg binary")
	flag.StringVar(&cfg.Aria2, "aria2", env("CKWEB_ARIA2", "aria2c"), "aria2c binary for module downloads; \"off\" uses the single-stream Go downloader")
	flag.StringVar(&cfg.Bootstrap, "bootstrap", env("CKWEB_BOOTSTRAP", "/usr/local/bin/bootstrap.sh"), "script that builds the inference stack in the volume")
	flag.StringVar(&cfg.Token, "token", env("CKWEB_TOKEN", ""), "shared access token; \"auto\" generates one and prints it (empty = open, localhost only)")
	maxGB := flag.Int64("max-upload-gb", 16, "largest accepted upload, in GB")
	flag.Parse()

	cfg.MaxUpload = *maxGB << 30
	cfg.Version = version
	cfg.Dev = cfg.Mode == "dev"
	if cfg.ClipsDir == "" {
		cfg.ClipsDir = filepath.Join(cfg.Root, "ClipsForInference")
	}
	if cfg.Python == "" {
		cfg.Python = pickPython(cfg.Root)
	}
	if err := os.MkdirAll(cfg.ClipsDir, 0o775); err != nil {
		log.Fatalf("clips dir: %v", err)
	}

	registerProvider(CorridorKey2Provider{})
	registerProvider(MockProvider{})
	if cfg.Dev {
		*providerFlag = "mock"
	}
	providerChoice = *providerFlag
	p, why := pickProvider(*providerFlag)
	if p == nil {
		log.Fatalf("provider: %s", why)
	}
	setProvider(p)

	ui, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(ui)))
	mux.HandleFunc("/api/health", handleHealth)
	mux.HandleFunc("/api/modules", handleModules)
	mux.HandleFunc("/api/modules/", handleModuleRoutes)
	mux.HandleFunc("/api/providers", handleProviders)
	mux.HandleFunc("/api/provision", handleProvision)
	mux.HandleFunc("/api/clips", handleClips)
	mux.HandleFunc("/api/clips/", handleClipRoutes)
	mux.HandleFunc("/api/upload", handleUpload)
	mux.HandleFunc("/api/jobs", handleJobs)
	mux.HandleFunc("/api/jobs/", handleJobRoutes)
	mux.HandleFunc("/api/events", handleEvents)

	queue.start()
	go reportFrameTraffic()

	if cfg.Token == "auto" {
		cfg.Token = storedToken()
	}

	log.Printf("ck-web %s on %s", cfg.Version, cfg.Addr)
	log.Printf("  mode     %s", cfg.Mode)
	log.Printf("  provider %s (%s)", p.ID(), why)
	log.Printf("  root     %s", cfg.Root)
	log.Printf("  clips    %s", cfg.ClipsDir)
	log.Printf("  python   %s", cfg.Python)
	if cfg.Dev {
		log.Printf("  modules  downloads disabled by -mode dev")
	} else if p, ok := aria2Available(); ok {
		log.Printf("  fetch    %s, %d connections per file", p, aria2MaxConn)
	} else if cfg.Aria2 == "off" {
		log.Printf("  fetch    single-stream Go downloader (-aria2 off)")
	} else {
		log.Printf("  fetch    single-stream Go downloader — %s not on PATH, installs will be slow", cfg.Aria2)
	}
	// Last, so the decision is printed under the rest of the banner, and after
	// the queue is running so the jobs it files have a worker to pick them up.
	logProvision(autoProvision())

	if cfg.Token == "" {
		log.Printf("  auth     DISABLED — bind to localhost and tunnel, or set CKWEB_TOKEN")
		log.Printf("  open     %s", pageURL(cfg.Addr, ""))
	} else {
		log.Printf("  auth     token required")
		log.Printf("  token    %s", cfg.Token)
		log.Printf("  open     %s", pageURL(cfg.Addr, cfg.Token))
	}
	log.Fatal(http.ListenAndServe(cfg.Addr, logging(auth(mux))))
}

// newToken is for -token auto: a box reachable from more than localhost needs
// a secret, and asking the operator to invent one means either a weak one or
// no token at all. This one is printed at startup, inside the ready-made URL.
func newToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("ck-web: could not generate a token: %v", err)
	}
	return hex.EncodeToString(b)
}

// tokenPath is where an -token auto secret is kept: beside the clips, which
// is the volume on a rented box, so it survives the container being replaced.
func tokenPath() string { return filepath.Join(filepath.Dir(cfg.ClipsDir), ".ck-web-token") }

// storedToken reads the generated token back, or makes one and saves it.
//
// A fresh secret on every restart looks like security and is the opposite of
// it in practice: the link in the console scrollback, the one in a bookmark
// and the one a login banner prints all stop working, so the operator is
// trained to go hunting for the new one — or to run without a token at all.
// The secret outlives the process instead, and -token <value> still overrides.
func storedToken() string {
	path := tokenPath()
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t
		}
	}
	t := newToken()
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		// Not fatal: the token works for this run, it just will not be the
		// same one next time, so say which it is going to be.
		log.Printf("ck-web: could not save the token to %s (%v) — it will change on restart", path, err)
	}
	return t
}

// pageURL renders the link to paste into a browser, token included, because
// the one question a fresh container cannot answer from the UI is what the
// token is.
func pageURL(addr, token string) string {
	host, port := addr, ""
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host, port = addr[:i], addr[i+1:]
	}
	// ":8080", "0.0.0.0:8080" and "[::]:8080" are all "this machine" to the
	// operator reading the log; only they know the routable name.
	switch host {
	case "", "0.0.0.0", "[::]", "::":
		host = "localhost"
	}
	u := "http://" + host
	if port != "" {
		u += ":" + port
	}
	u += "/"
	if token != "" {
		u += "?t=" + url.QueryEscape(token)
	}
	return u
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// pickPython prefers the project venv, since that is where torch lives.
func pickPython(root string) string {
	for _, c := range []string{"venv/bin/python", ".venv/bin/python", "venv/Scripts/python.exe"} {
		p := filepath.Join(root, c)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "python3"
}

// auth gates everything but the static shell behind the token. The browser
// sends it as a header; <img>/<video> and EventSource cannot, so a ?t=
// query param is accepted too.
func auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.Token == "" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-CK-Token")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if got == "" {
			got = r.URL.Query().Get("t")
		}
		if got != cfg.Token {
			http.Error(w, "bad or missing token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logged wraps a response so the status and the size can be reported after
// the handler has finished with it.
type logged struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *logged) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *logged) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush and the rest are needed because the SSE handler asserts them.
func (w *logged) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// frameTraffic collapses the scrubber's requests. A pass over a 348-frame
// clip is 348 lines of identical shape; the interesting facts are how many
// arrived, how many cost a render and how many were answered 304, so they are
// counted and summarised instead of printed one by one.
var frameTraffic struct {
	sync.Mutex
	n, served, notModified, failed int
	bytes                          int
}

func countFrame(status, bytes int) {
	frameTraffic.Lock()
	frameTraffic.n++
	switch {
	case status == http.StatusNotModified:
		frameTraffic.notModified++
	case status >= 400:
		frameTraffic.failed++
	default:
		frameTraffic.served++
		frameTraffic.bytes += bytes
	}
	frameTraffic.Unlock()
}

// reportFrameTraffic prints one summary line while frames are moving, and
// nothing at all when they are not.
func reportFrameTraffic() {
	for range time.Tick(5 * time.Second) {
		frameTraffic.Lock()
		n, served, nm, failed, b := frameTraffic.n, frameTraffic.served,
			frameTraffic.notModified, frameTraffic.failed, frameTraffic.bytes
		frameTraffic.n, frameTraffic.served, frameTraffic.notModified = 0, 0, 0
		frameTraffic.failed, frameTraffic.bytes = 0, 0
		frameTraffic.Unlock()
		if n == 0 {
			continue
		}
		st := frameStats()
		line := fmt.Sprintf("frames   %d asked in 5s — %d sent (%s), %d from the browser cache",
			n, served, humanBytes(int64(b)), nm)
		if failed > 0 {
			line += fmt.Sprintf(", %d failed", failed)
		}
		log.Printf("%s; server cache %v frames, %v rendered since boot",
			line, st["cached"], st["rendered"])
	}
}

// isFrameRequest matches /api/clips/<name>/frame, the one route the scrubber
// hits hundreds of times in a row.
func isFrameRequest(path string) bool {
	return strings.HasPrefix(path, "/api/clips/") && strings.HasSuffix(path, "/frame")
}

// logging narrates the HTTP side. Everything but the frame flood gets a line
// with its status, size and how long it took, because "the server went down"
// and "the server answered 404 in 1 ms" look identical from a browser.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" {
			// Long-lived by design: a start and an end, not a duration.
			log.Printf("%s %s stream opened (%d watching)", r.Method, r.URL.Path, subscribers()+1)
			next.ServeHTTP(w, r)
			log.Printf("%s %s stream closed (%d watching)", r.Method, r.URL.Path, subscribers())
			return
		}

		start := time.Now()
		lw := &logged{ResponseWriter: w}
		next.ServeHTTP(lw, r)
		if lw.status == 0 {
			lw.status = http.StatusOK
		}
		took := time.Since(start)

		if isFrameRequest(r.URL.Path) {
			countFrame(lw.status, lw.bytes)
			// A frame that failed is not routine, so it is said out loud even
			// though its siblings are counted.
			if lw.status >= 400 {
				log.Printf("%s %s?%s %d in %v", r.Method, r.URL.Path, r.URL.RawQuery,
					lw.status, took.Round(time.Millisecond))
			}
			return
		}

		q := ""
		if r.URL.RawQuery != "" {
			// The token is in here on <img> and EventSource URLs; never log it.
			if v := r.URL.Query(); v.Get("t") != "" {
				v.Set("t", "…")
				q = "?" + v.Encode()
			} else {
				q = "?" + r.URL.RawQuery
			}
		}
		log.Printf("%s %s%s %d %s in %v", r.Method, r.URL.Path, q, lw.status,
			humanBytes(int64(lw.bytes)), took.Round(time.Millisecond))
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	ap := activeProvider()
	ready, detail := ap.Ready()
	fetcher := "go"
	if _, ok := aria2Available(); ok {
		fetcher = fmt.Sprintf("aria2c x%d", aria2MaxConn)
	}
	writeJSON(w, map[string]any{
		"ok":            true,
		"version":       cfg.Version,
		"mode":          cfg.Mode,
		"dev":           cfg.Dev,
		"root":          cfg.Root,
		"clips":         cfg.ClipsDir,
		"gpu":           gpuInfo(),
		"auth":          cfg.Token != "",
		"fetcher":       fetcher,
		"provider":      ap.ID(),
		"providerTitle": ap.Title(),
		"providerReady": ready,
		"providerWhy":   detail,
		"provision":     provisionStatus(),
		"frames":        frameStats(),
	})
}
