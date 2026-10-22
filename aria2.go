package main

// Module downloads go through aria2c when it is on PATH. A single HTTP stream
// to Hugging Face tops out well below what a rented box's link can do — the
// GLM and Qwen pulls sat at a fraction of the available bandwidth for hours —
// and the only fix that works is more connections to the same object.
//
// Go's net/http cannot do that on its own: one request is one stream. Rather
// than reimplement ranged, resumable, multi-connection fetching here, this
// shells out to the tool that already does it, and keeps the pure-Go path as
// the fallback so the image still works without it.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// aria2MaxConn is a hard bound in aria2 itself, not a preference:
// OptionHandlerFactory.cc declares --max-connection-per-server as 1..16 and
// aria2c exits with an option error on 17, taking the whole install with it.
// --split cannot route around it either, since the split count is clamped by
// the per-server limit for a single-host download.
const aria2MaxConn = 16

// aria2Available reports whether the binary named by -aria2 can be run. It is
// resolved once at startup so the Modules page can say which downloader a
// given install will use before the user commits to 37 GB.
func aria2Available() (string, bool) {
	if cfg.Aria2 == "" || cfg.Aria2 == "off" {
		return "", false
	}
	p, err := exec.LookPath(cfg.Aria2)
	if err != nil {
		return "", false
	}
	return p, true
}

// aria2Download fetches one URL to out, resuming whatever .part is already
// there. It returns the file still named .part: the caller owns the rename, so
// a half-finished file is never mistaken for an installed one.
func aria2Download(ctx context.Context, url, out string, log func(string)) error {
	dir, base := filepath.Split(out)
	if dir == "" {
		dir = "."
	}
	part := base + ".part"

	args := []string{
		"--dir=" + strings.TrimSuffix(dir, string(os.PathSeparator)),
		"--out=" + part,
		fmt.Sprintf("--max-connection-per-server=%d", aria2MaxConn),
		fmt.Sprintf("--split=%d", aria2MaxConn),
		// Below ~1 MB a piece costs more in request overhead than it wins.
		"--min-split-size=1M",
		// Resume across a cancelled job or a restarted container: a 37 GB
		// module that died at 90% must not start over.
		"--continue=true",
		"--allow-overwrite=true",
		// Without this a retry writes out.part.1 and the rename below picks
		// up the stale, short file.
		"--auto-file-renaming=false",
		"--file-allocation=none",
		"--max-tries=5",
		"--retry-wait=2",
		"--summary-interval=1",
		"--console-log-level=warn",
		"--show-console-readout=false",
		url,
	}

	cmd := exec.CommandContext(ctx, cfg.Aria2, args...)
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return err
	}
	pw.Close() // the child now holds the only writer, so the read below ends

	name := strings.TrimSuffix(base, ".part")
	done := make(chan struct{})
	var tail string
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		last := time.Now().Add(-time.Second)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if p, ok := parseAria2Progress(line); ok {
				// Same one-a-second throttle as the Go downloader: aria2
				// prints a summary every interval and a 10-hour pull would
				// otherwise bury every other log line.
				if time.Since(last) < time.Second {
					continue
				}
				last = time.Now()
				log("    " + name + " — " + p)
				continue
			}
			tail = line // kept for the error message if aria2 exits non-zero
		}
	}()

	waitErr := cmd.Wait()
	pr.Close()
	<-done

	if waitErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr // cancelled: the .part stays, -continue resumes it
		}
		if tail != "" {
			return fmt.Errorf("aria2c: %s", tail)
		}
		return fmt.Errorf("aria2c: %w", waitErr)
	}

	full := filepath.Join(dir, part)
	fi, err := os.Stat(full)
	if err != nil {
		return fmt.Errorf("aria2c reported success but wrote nothing to %s", full)
	}
	if fi.Size() == 0 {
		os.Remove(full)
		return fmt.Errorf("aria2c wrote an empty file for %s", name)
	}
	return os.Rename(full, out)
}

// aria2 prints its summary as
//
//	[#2089b0 400MiB/2.5GiB(15%) CN:16 DL:85MiB ETA:25s]
//
// and the pieces worth showing are the sizes, the percentage, the connection
// count (proof the split actually happened) and the rate.
var aria2Line = regexp.MustCompile(
	`\[#[0-9a-f]+ +([0-9.]+[KMGT]?i?B)/([0-9.]+[KMGT]?i?B)\((\d+)%\)(?: +CN:(\d+))?(?: +DL:([0-9.]+[KMGT]?i?B))?(?: +ETA:(\S+?))?\]`)

func parseAria2Progress(line string) (string, bool) {
	m := aria2Line.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	s := fmt.Sprintf("%s / %s (%s%%)", m[1], m[2], m[3])
	if m[5] != "" {
		s += " · " + m[5] + "/s"
	}
	if m[4] != "" {
		s += " · " + m[4] + " conn"
	}
	if m[6] != "" {
		s += " · ETA " + m[6]
	}
	return s, true
}
