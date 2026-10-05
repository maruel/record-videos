// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestConstructFilterGraph(t *testing.T) {
	for _, s := range validStyles {
		t.Run(string(s), func(t *testing.T) {
			g := constructFilterGraph(s, 640, 480).String()
			for _, want := range []string{"[0:v]", "signalstats", "metadata=print:key=lavfi.signalstats.YAVG", "file='pipe\\:3'", "[out]"} {
				if !strings.Contains(g, want) {
					t.Errorf("filter graph lacks %q: %s", want, g)
				}
			}
		})
	}
}

func TestBuildFFMPEGCmd(t *testing.T) {
	for _, mpjpeg := range []bool{false, true} {
		t.Run(map[bool]string{false: "hls", true: "hls_and_mjpeg"}[mpjpeg], func(t *testing.T) {
			o := &ffmpegOptions{
				src: "tcp://camera:8081", w: 640, h: 480, fps: 25,
				s: "normal_no_mask", codec: "libx264", level: "warning", mpjpeg: mpjpeg,
			}
			a, err := buildFFMPEGCmd(o)
			if err != nil {
				t.Fatal(err)
			}
			if a[0] != "ffmpeg" || !slices.Contains(a, "all.m3u8") {
				t.Fatalf("missing executable or HLS playlist: %q", a)
			}
			if slices.Contains(a, "pipe:4") != mpjpeg {
				t.Errorf("MJPEG pipe enabled incorrectly: %q", a)
			}
			if slices.Contains(a, "-t") {
				t.Error("continuous recording has a duration limit")
			}
			for flag, want := range map[string]string{"-i": "tcp://camera:8081", "-framerate": "25", "-c:v": "libx264", "-map": "[out]"} {
				if flag == "-map" && mpjpeg {
					want = "[outHLS]"
				}
				i := slices.Index(a, flag)
				if i < 0 || i+1 >= len(a) || a[i+1] != want {
					t.Errorf("%s: want %q in %q", flag, want, a)
				}
			}
		})
	}
	t.Run("mask_and_duration", func(t *testing.T) {
		a, err := buildFFMPEGCmd(&ffmpegOptions{
			src: "tcp://camera:8081", mask: "mask.png", w: 640, h: 480, fps: 25,
			s: "normal", codec: "libx264", level: "warning", d: 1500 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(a, "mask.png") || slices.Contains(a, "color=color=white:size=32x32") {
			t.Errorf("mask not used: %q", a)
		}
		i := slices.Index(a, "-t")
		if i < 0 || i+1 >= len(a) || a[i+1] != "1.5s" {
			t.Errorf("incorrect recording duration: %q", a)
		}
	})
}
