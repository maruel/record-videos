// Copyright 2026 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeVideo(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"all.m3u8", "segment.ts", "private.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("recording"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("valid", func(t *testing.T) {
		for _, tc := range []struct{ name, cache string }{
			{"all.m3u8", "no-store"},
			{"segment.ts", "public"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := httptest.NewRecorder()
				serveVideo(root, w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/raw/"+tc.name, nil))
				if w.Code != http.StatusOK || w.Body.String() != "recording" {
					t.Errorf("got status %d, body %q", w.Code, w.Body.String())
				}
				if !strings.Contains(w.Header().Get("Cache-Control"), tc.cache) {
					t.Errorf("incorrect cache policy: %v", w.Header())
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		for _, path := range []string{
			"/raw/", "/raw/../all.m3u8", "/raw/%2e%2e%2fall.m3u8",
			"/raw/%252e%252e%252fall.m3u8", "/raw/sub/segment.ts",
			"/raw/sub%5csegment.ts", "/raw/private.txt", "/raw/missing.ts",
			"/raw/%25invalid.ts",
		} {
			t.Run(path, func(t *testing.T) {
				w := httptest.NewRecorder()
				serveVideo(root, w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
				if w.Code != http.StatusNotFound {
					t.Errorf("got status %d, want 404", w.Code)
				}
			})
		}
	})
}
