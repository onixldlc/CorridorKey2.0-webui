package main

// Serving one frame of one view, cheaply enough to drag a scrubber through.
//
// Every frame the viewer shows costs real work: an EXR is 16-bit linear and no
// browser reads it, so ffmpeg converts it, and a comp also seeks the plate and
// blends the two. Dragging the scrubber used to fire one of those per input
// event, with Cache-Control: no-store on the way back, so scrubbing forked
// dozens of ffmpegs at once, none of the answers were ever reused, and the box
// fell over.
//
// Three things fix that, and none of them is a cache in the browser's
// localStorage — a 4K frame does not fit in a 5 MB quota that only holds
// strings:
//
//  1. A hard ceiling on how many frames are rendered at once, so a flood of
//     requests queues instead of forking.
//  2. One render per distinct frame: requests that arrive for a frame already
//     being rendered wait for that render rather than starting another.
//  3. Answers that are cacheable. An ETag derived from the source file's size
//     and mtime means the second visit to a frame is a 304 with no ffmpeg at
//     all, and the browser keeps the bytes itself — in its own HTTP cache,
//     which is measured in hundreds of megabytes and evicts without help.
//
// A small LRU in front of all that holds the bytes of recently rendered
// frames, so scrubbing back and forth over the same stretch does not reconvert
// anything even on a cold browser cache.

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// frameCacheBytes is the LRU's ceiling. A 1600-wide preview is a few hundred
// KB, so this holds a few hundred frames: enough for the stretch of a clip
// someone is actually working on.
const frameCacheBytes = 192 << 20

// maxFrameRenders is how many frames may be rendered at once. Each one is an
// ffmpeg process or two, so this is about not turning a scrub into a fork
// bomb; the queue behind it is what makes the UI feel slow rather than the box
// feel dead.
var maxFrameRenders = func() int {
	n := runtime.NumCPU() / 2
	if n < 2 {
		return 2
	}
	if n > 4 {
		return 4
	}
	return n
}()

var renderSem = make(chan struct{}, maxFrameRenders)

// frameCacheControl lets the browser hold a frame for an hour without asking,
// and revalidate cheaply after that. private, because a token in the URL is
// the only thing standing between these pictures and anyone else.
const frameCacheControl = "private, max-age=3600, stale-while-revalidate=86400"

// renderedFrame is what a request for a frame produces.
type renderedFrame struct {
	Body  []byte
	Type  string
	ETag  string
	Cache bool // false for an error answer, which is never cached
}

func (f renderedFrame) size() int64 { return int64(len(f.Body)) }

// ── the LRU ──────────────────────────────────────────────────────────────────

type frameCache struct {
	mu    sync.Mutex
	max   int64
	used  int64
	items map[string]*list.Element
	order *list.List // front is newest
}

type cacheItem struct {
	key   string
	frame renderedFrame
}

var frames = newFrameCache(frameCacheBytes)

func newFrameCache(max int64) *frameCache {
	return &frameCache{max: max, items: map[string]*list.Element{}, order: list.New()}
}

func (c *frameCache) get(key string) (renderedFrame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return renderedFrame{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheItem).frame, true
}

func (c *frameCache) put(key string, f renderedFrame) {
	if !f.Cache || f.size() == 0 || f.size() > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.used -= el.Value.(*cacheItem).frame.size()
		c.order.Remove(el)
	}
	c.items[key] = c.order.PushFront(&cacheItem{key: key, frame: f})
	c.used += f.size()
	for c.used > c.max {
		el := c.order.Back()
		if el == nil {
			break
		}
		it := el.Value.(*cacheItem)
		c.used -= it.frame.size()
		c.order.Remove(el)
		delete(c.items, it.key)
	}
}

func (c *frameCache) stats() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.used
}

// ── one render per frame ─────────────────────────────────────────────────────

type flight struct {
	wg    sync.WaitGroup
	frame renderedFrame
	err   error
}

var (
	flightMu sync.Mutex
	flights  = map[string]*flight{}
)

// renderOnce runs fn for key, or waits for the run already under way. Two
// browsers scrubbing the same clip, or one browser that re-requested a frame
// it had already asked for, cost one conversion between them.
func renderOnce(key string, fn func() (renderedFrame, error)) (renderedFrame, error) {
	if f, ok := frames.get(key); ok {
		return f, nil
	}

	flightMu.Lock()
	if fl, ok := flights[key]; ok {
		flightMu.Unlock()
		fl.wg.Wait()
		return fl.frame, fl.err
	}
	fl := &flight{}
	fl.wg.Add(1)
	flights[key] = fl
	flightMu.Unlock()

	renderSem <- struct{}{}
	fl.frame, fl.err = fn()
	<-renderSem

	if fl.err == nil {
		frames.put(key, fl.frame)
	}
	flightMu.Lock()
	delete(flights, key)
	flightMu.Unlock()
	fl.wg.Done()
	return fl.frame, fl.err
}

// ── the request ──────────────────────────────────────────────────────────────

// frameRequest is everything that decides what a frame looks like. It is also
// the cache key, so every field has to be in it.
type frameRequest struct {
	Clip    string
	View    string
	Index   int
	BG      string
	Despill int // 0-10, as the slider sends it
	Source  string
	Stamp   string // size and mtime of the source, so a re-key busts the key
}

func (q frameRequest) key() string {
	raw := fmt.Sprintf("%s|%s|%d|%s|%d|%s|%s",
		q.Clip, q.View, q.Index, q.BG, q.Despill, q.Source, q.Stamp)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

func (q frameRequest) etag() string { return `"` + q.key() + `"` }

// stampOf identifies a file by size and mtime, which is enough to notice a
// re-key without hashing hundreds of megabytes of EXR.
func stampOf(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return strconv.FormatInt(st.Size(), 36) + "-" + strconv.FormatInt(st.ModTime().UnixNano(), 36)
}

// planFrame works out which file answers a request, and what identifies it.
func planFrame(r *http.Request, clip string) (frameRequest, error) {
	view := q(r, "view", "matte")
	idx, _ := strconv.Atoi(q(r, "i", "0"))
	if idx < 0 {
		idx = 0
	}
	req := frameRequest{Clip: clip, View: view, Index: idx}

	// A comp is identified by the matte frame it is built from, plus the two
	// controls that change how it looks.
	names, dir := frameList(clip, compSource(view))
	if len(names) == 0 {
		if view == "comp" {
			return req, fmt.Errorf("no matte yet — key the clip first")
		}
		return req, fmt.Errorf("no frames in " + view)
	}
	if req.Index >= len(names) {
		req.Index = len(names) - 1
	}
	req.Source = filepath.Join(dir, names[req.Index])
	req.Stamp = stampOf(req.Source)
	if view == "comp" {
		req.BG = q(r, "bg", "checker")
		req.Despill, _ = strconv.Atoi(q(r, "despill", "5"))
		req.Despill = clampInt(req.Despill, 0, 10)
		// The plate is half the picture, so its identity belongs in the key.
		if c := inspectClip(clip); c.Input != "" && !strings.HasSuffix(c.Input, "/") {
			req.Stamp += "+" + stampOf(filepath.Join(cfg.ClipsDir, clip, c.Input))
		}
	}
	return req, nil
}

// renderFrameRequest produces the bytes for a request, from the cache, from a
// render already running, or from ffmpeg.
func renderFrameRequest(req frameRequest) (renderedFrame, error) {
	return renderOnce(req.key(), func() (renderedFrame, error) {
		if req.View == "comp" {
			img, err := compositeFrame(req.Clip, req.Index, req.BG,
				Params{Despill: req.Despill}.despillStrength())
			if err != nil {
				return renderedFrame{}, err
			}
			var buf bytes.Buffer
			// JPEG, not PNG: a comp is a photograph of a plate, and at quality
			// 88 it is five to ten times smaller than the PNG for a picture
			// nobody is pulling a matte from. The matte itself stays lossless.
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
				return renderedFrame{}, err
			}
			return renderedFrame{Body: buf.Bytes(), Type: "image/jpeg",
				ETag: req.etag(), Cache: true}, nil
		}

		if strings.EqualFold(filepath.Ext(req.Source), ".exr") {
			// Capped rather than full size: a 4K matte is 8 MB of PNG per
			// frame, and the viewer is showing it in a browser window.
			out, err := frameAsPNG(req.Source, compMaxWidth, 0)
			if err != nil {
				return renderedFrame{}, err
			}
			return renderedFrame{Body: out, Type: "image/png",
				ETag: req.etag(), Cache: true}, nil
		}

		out, err := os.ReadFile(req.Source)
		if err != nil {
			return renderedFrame{}, err
		}
		ctype := "image/png"
		switch strings.ToLower(filepath.Ext(req.Source)) {
		case ".jpg", ".jpeg":
			ctype = "image/jpeg"
		case ".tif", ".tiff":
			ctype = "image/tiff"
		}
		return renderedFrame{Body: out, Type: ctype, ETag: req.etag(), Cache: true}, nil
	})
}
