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
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
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

// startServer starts the web server.
//
// It serves:
// - /mjpeg to retransmit mime encoded mjpeg.
// - /videos HTML page that contains <video> tags for each .m3u8 file found.
// - /list HTML page with a link to each .m3u8 file found.
// - /raw/ to serve individual .m3u8 and .ts files
func startServer(ctx context.Context, addr string, r io.Reader, root string) error {
	m := http.ServeMux{}
	tm := &teeMimePart{}
	go func() {
		err2 := tm.listen(ctx, r, "ffmpeg")
		slog.Info("teeMimePart", "msg", "exit", "err", err2)
	}()

	go func() {
		ctx2, cancel := context.WithCancel(ctx)
		defer cancel()
		ch := tm.relay(ctx)
		select {
		case pkt := <-ch:
			slog.Info("ready", "bytes", len(pkt.b))
		case <-ctx2.Done():
		}
	}()

	// MJPEG stream
	m.HandleFunc("GET /mjpeg", func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		slog.Info("http", "remote", req.RemoteAddr)
		mw := multipart.NewWriter(w)
		defer mw.Close()
		h := w.Header()
		h.Set("Content-Type", "multipart/x-mixed-replace;boundary="+mw.Boundary())
		h.Set("Connection", "close")
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.Set("Pragma", "no-cache")
		h.Set("Expires", "0")
		w.WriteHeader(200)
		ctx := req.Context()
		ch := tm.relay(ctx)
		done := ctx.Done()
		for i := 0; ctx.Err() == nil; i++ {
			select {
			case p := <-ch:
				slog.Debug("http", "remote", req.RemoteAddr, "i", i, "b", len(p.b))
				fw, err := mw.CreatePart(p.hdr)
				if err != nil {
					slog.Error("http", "remote", req.RemoteAddr, "err", err)
					break
				}
				if _, err := fw.Write(p.b); err != nil {
					slog.Error("http", "remote", req.RemoteAddr, "err", err)
					break
				}
			case <-done:
			}
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
	go func() {
		err2 := s.Serve(l)
		slog.Info("http", "msg", "exit", "err", err2)
	}()
	// TODO: clean shutdown.
	//s.Shutdown(context.Background())
	return nil
}
