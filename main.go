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

	if cfg.Token == "auto" {
		cfg.Token = newToken()
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

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
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
