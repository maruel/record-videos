// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"context"
	_ "embed"
	"html/template"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	//go:embed html/videos.html
	videosHTML []byte

	//go:embed html/list.html
	listHTML []byte

	// Injected data to speed up page load, versus having to do an API call.
	dataTmpl = template.Must(template.New("").Parse("<script>'use strict';const data = {{.}};</script>"))
)

// broadcastFrames broadcast MPJPEG frames to listeners.
type broadcastFrames struct {
	mu        sync.Mutex
	lastFrame []byte
	listeners []chan []byte
}

// listen reads ffmpeg's mpjpeg mime stream, decodes it, then send it to
// readers.
func (b *broadcastFrames) listen(ctx context.Context, r io.Reader) {
	mr := multipart.NewReader(r, "ffmpeg")
	for i := 0; ctx.Err() == nil; i++ {
		p, err := mr.NextPart()
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
		frame, err := io.ReadAll(p)
		if err != nil {
			return
		}
		// First frame read.
		if i == 0 {
			slog.Info("ready")
		}
		//slog.Debug("mjpeg", "i", i, "b", len(frame))
		// Expected p.Header:
		//	Content-type: image/jpeg
		//	Content-length: 1234
		b.mu.Lock()
		b.lastFrame = frame
		l := make([]chan []byte, len(b.listeners))
		copy(l, b.listeners)
		b.mu.Unlock()
		for _, x := range l {
			select {
			case x <- frame:
			default:
			}
		}
	}
}

func (b *broadcastFrames) relay(ctx context.Context) iter.Seq[[]byte] {
	ch := make(chan []byte, 1)
	b.mu.Lock()
	b.listeners = append(b.listeners, ch)
	frame := b.lastFrame
	b.mu.Unlock()
	return func(yield func([]byte) bool) {
		defer func() {
			b.mu.Lock()
			for i := range b.listeners {
				if b.listeners[i] == ch {
					copy(b.listeners[i:], b.listeners[i+1:])
					b.listeners = b.listeners[:len(b.listeners)-1]
					break
				}
			}
			b.mu.Unlock()
		}()
		if ctx.Err() == nil && yield(frame) {
			for {
				select {
				case <-ctx.Done():
					return
				case frame := <-ch:
					if !yield(frame) {
						return
					}
				}
			}
		}
	}
}

// startServer starts the web server.
//
// It serves:
// - /mjpeg to retransmit mime encoded mjpeg.
// - /videos HTML page that contains <video> tags for each .m3u8 file found.
// - /list HTML page with a link to each .m3u8 file found.
// - /raw/ to serve individual .m3u8 and .ts files
func startServer(ctx context.Context, addr string, r io.Reader, root string) error {
	m := http.ServeMux{}
	bf := &broadcastFrames{}
	go bf.listen(ctx, r)

	// MJPEG stream
	m.HandleFunc("GET /mjpeg", func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		slog.Info("http", "remote", req.RemoteAddr)
		mw := multipart.NewWriter(w)
		defer mw.Close()
		h := w.Header()
		h.Set("Content-Type", "multipart/x-mixed-replace;boundary="+mw.Boundary())
		//h.Set("Content-Type", "multipart/x-mixed-replace;boundary=FRAME")
		h.Set("Connection", "close")
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.Set("Pragma", "no-cache")
		h.Set("Expires", "0")
		i := 0
		for frame := range bf.relay(req.Context()) {
			slog.Debug("http", "remote", req.RemoteAddr, "i", i, "b", len(frame))
			fw, err := mw.CreatePart(textproto.MIMEHeader{
				"Content-Type":   []string{"image/jpeg"},
				"Content-Length": []string{strconv.Itoa(len(frame))},
			})
			if err != nil {
				break
			}
			if _, err := fw.Write(frame); err != nil {
				break
			}
			i++
		}
		slog.Info("http", "remote", req.RemoteAddr, "done", true, "d", time.Since(start).Round(100*time.Millisecond))
	})

	// Video serving.
	m.HandleFunc("GET /raw/", func(w http.ResponseWriter, req *http.Request) {
		path, err2 := url.QueryUnescape(req.URL.Path)
		if err2 != nil {
			slog.Error("http", "path", req.URL.Path)
			http.Error(w, "Invalid path", 404)
			return
		}
		f := path[len("/raw/"):]
		// Limit to not path, only .m3u8 and .ts.
		if strings.Contains(f, "/") || strings.Contains(f, "\\") || strings.Contains(f, "..") || (!strings.HasSuffix(f, ".m3u8") && !strings.HasSuffix(f, ".ts")) {
			slog.Error("http", "path", req.URL.Path)
			http.Error(w, "Invalid path", 404)
			return
		}

		// Cache for a long time, the exception is m3u8 since it could be a live
		// playlist.
		if h := w.Header(); strings.HasSuffix(f, ".m3u8") {
			h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
			h.Set("Pragma", "no-cache")
			h.Set("Expires", "0")
		} else {
			h.Set("Cache-Control", "public, max-age=86400")
		}
		http.ServeFile(w, req, filepath.Join(root, f))
	})

	// HTML
	m.HandleFunc("GET /list", func(w http.ResponseWriter, req *http.Request) {
		var files []string
		offset := len(root) + 1
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if !d.IsDir() && strings.HasSuffix(path, ".m3u8") || strings.HasSuffix(path, ".ts") {
				files = append(files, path[offset:])
			}
			return nil
		})
		sort.Strings(files)
		h := w.Header()
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.Set("Pragma", "no-cache")
		h.Set("Expires", "0")
		h.Set("Content-Type", "text/html; charset=utf-8")
		if _, err2 := w.Write(listHTML); err2 != nil {
			return
		}
		_ = dataTmpl.Execute(w, map[string]any{"files": files})
	})
	m.HandleFunc("GET /videos", func(w http.ResponseWriter, req *http.Request) {
		var files []string
		offset := len(root) + 1
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if !d.IsDir() && strings.HasSuffix(path, ".m3u8") {
				files = append(files, path[offset:])
			}
			return nil
		})
		sort.Strings(files)
		h := w.Header()
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.Set("Pragma", "no-cache")
		h.Set("Expires", "0")
		h.Set("Content-Type", "text/html; charset=utf-8")
		if _, err2 := w.Write(videosHTML); err2 != nil {
			return
		}
		_ = dataTmpl.Execute(w, map[string]any{"files": files})
	})

	m.HandleFunc("GET /", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/" {
			http.Redirect(w, req, "videos", http.StatusFound)
			return
		}
		slog.Error("http", "path", req.URL.Path)
		http.Error(w, "Not found", http.StatusNotFound)
	})
	s := http.Server{
		Handler:      &m,
		BaseContext:  func(net.Listener) context.Context { return ctx },
		ReadTimeout:  10. * time.Second,
		WriteTimeout: 366 * 24 * time.Hour,
		IdleTimeout:  10. * time.Second,
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	slog.Info("http", "addr", l.Addr())
	go s.Serve(l)
	// TODO: clean shutdown.
	//s.Shutdown(context.Background())
	return nil
}
