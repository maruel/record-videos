// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// motionOptions is the options for motion detection and recording.
type motionOptions struct {
	// yThreshold determines the motion sensitivity as per the Y (from YUV)
	// average pixel brightness when two frames are substracted and then an edge
	// detection algorithm is ran over.
	yThreshold float64
	// preCapture is the duration to record before the motion is detected.
	preCapture time.Duration
	// postCapture is the duration to record after the motion is timed out.
	postCapture time.Duration
	// motionExpiration is the duration after which a motion is timed out.
	motionExpiration time.Duration
	// ignoreFirstFrames ignores motion detection from these initial frames. Many
	// cameras will auto-focus and cause a lot of artificial motion when starting
	// up.
	ignoreFirstFrames int
	// ignoreFirstMoments ignores motion detection when the stream starts.
	ignoreFirstMoments time.Duration

	// onEventStart is a script to run upon motion detection.
	onEventStart string
	// onEventEnd is a script to run upon motion timeout.
	onEventEnd string
	// webhook is a webhook to call with application/json content
	// `{"motion":true}` upon motion and a second time with false upon timeout.
	webhook string

	_ struct{}
}

// yLevel is the level of Y channel average on the image, which is the
// amount of edge movements detected.
type yLevel struct {
	frame int
	t     time.Time
	yavg  float64
}

// motionEvent is a processed yLevel to determine when motion started and
// stopped.
type motionEvent struct {
	t     time.Time
	start bool
}

// processMetadata processes metadata from ffmpeg's metadata:print filter.
//
// It expects data in the form:
//
//	frame:1336 pts:1336    pts_time:53.44
//	lavfi.signalstats.YAVG=0.213281
func processMetadata(start time.Time, r io.Reader, ch chan<- yLevel) error {
	b := bufio.NewScanner(r)
	frame := 0
	var ptsTime time.Duration
	yavg := 0.
	var err2 error
	for b.Scan() {
		l := b.Text()
		//slog.Debug("metadata", "l", l)
		if a, ok := strings.CutPrefix(l, "lavfi.signalstats.YAVG="); ok {
			if yavg, err2 = strconv.ParseFloat(a, 64); err2 != nil {
				slog.Error("metadata", "err", err2)
				return fmt.Errorf("unexpected metadata output: %q", l)
			}
			yavg = math.Round(yavg*100) * 0.01
			ch <- yLevel{frame: frame, t: start.Add(ptsTime).Round(100 * time.Millisecond), yavg: yavg}
			continue
		}
		f := strings.Fields(l)
		if len(f) != 3 || !strings.HasPrefix(f[0], "frame:") || !strings.HasPrefix(f[2], "pts_time:") {
			slog.Error("metadata", "f", f)
			return fmt.Errorf("unexpected metadata output: %q", l)
		}
		if frame, err2 = strconv.Atoi(f[0][len("frame:"):]); err2 != nil {
			slog.Error("metadata", "err", err2)
			return fmt.Errorf("unexpected metadata output: %q", l)
		}
		v := 0.
		if v, err2 = strconv.ParseFloat(f[2][len("pts_time:"):], 64); err2 != nil {
			slog.Error("metadata", "err", err2)
			return fmt.Errorf("unexpected metadata output: %q", l)
		}
		ptsTime = time.Duration(v * float64(time.Second))
	}
	_ = frame
	return b.Err()
}

// filterMotion converts raw Y data into motion detection events.
func filterMotion(ctx context.Context, mo *motionOptions, start time.Time, ch <-chan yLevel, events chan<- motionEvent) error {
	// TODO: Get the ready signal from MPJPEG reader!
	done := ctx.Done()
	var motionTimeout <-chan time.Time
	inMotion := false
	for {
		select {
		case <-done:
			return nil
		case l, ok := <-ch:
			if !ok {
				return nil
			}
			// Since we do not use printFilteredYAVGtoPipe anymore so we can use the
			// motion level output as a keep-alive, we need to filter out logs.
			if l.yavg > 0.1 {
				slog.Info("yLevel", "t", l.t.Format("2006-01-02T15:04:05.00"), "f", l.frame, "yavg", l.yavg)
			}
			if l.frame >= mo.ignoreFirstFrames && l.t.Sub(start) >= mo.ignoreFirstMoments && l.yavg >= mo.yThreshold {
				motionTimeout = time.After(mo.motionExpiration - time.Since(l.t))
				if !inMotion {
					inMotion = true
					events <- motionEvent{t: l.t, start: true}
				}
			}
		case t := <-motionTimeout:
			events <- motionEvent{t: t.Round(100 * time.Millisecond), start: false}
			inMotion = false

		case <-time.After(10 * time.Second):
			// It's dead jim. It can happen when the USB port hangs, or if the remote
			// TCP died. It's easier to just quit, and have systemd restart the
			// process.
			return errors.New("no events for more than 10s")
		}
	}
}

// m3u8Tmpl is the template to write a .m3u8 HLS playlist file.
var m3u8Tmpl = template.Must(template.New("").Parse(`#EXTM3U
#EXT-X-VERSION:6
#EXT-X-ALLOW-CACHE:YES
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-INDEPENDENT-SEGMENTS
{{range .}}#EXTINF:4.000000,
{{.}}
{{end}}`))

func findTSFiles(root string, start, end time.Time) ([]string, error) {
	// TODO: would be better to not load the whole directory list, or at least
	// partition per day or something.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, 8)
	s := start.Format("2006-01-02T15-04-05") + ".ts"
	e := end.Format("2006-01-02T15-04-05") + ".ts"
	for _, entry := range entries {
		if n := entry.Name(); strings.HasSuffix(n, ".ts") && n >= s && n <= e {
			out = append(out, n)
		}
	}
	slog.Debug("findTSFiles", "start", s, "end", e, "total", len(entries), "found", len(out))
	return out, err
}

// generateM3U8 writes a .m3u8 in a temporary file then renames it.
func generateM3U8(root string, t, start, end time.Time) error {
	files, err := findTSFiles(root, start, end)
	if err != nil || len(files) == 0 {
		return err
	}
	slog.Debug("generateM3U8", "t", t, "start", start, "end", end, "files", files)
	name := filepath.Join(root, t.Format("2006-01-02T15-04-05")+".m3u8")
	// #nosec G304
	f, err := os.Create(name + ".tmp")
	if err != nil {
		return err
	}
	err = m3u8Tmpl.Execute(f, files)
	if err2 := f.Close(); err == nil {
		err = err2
	}
	if err != nil {
		return err
	}
	return os.Rename(name+".tmp", name)
}

// runCmd runs a command and give it at most 1 minute to run.
func runCmd(ctx context.Context, a string) error {
	slog.Info("exec", "args", a)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, a)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// processMotion reacts to motion start and stop events.
func processMotion(ctx context.Context, mo *motionOptions, root string, ch <-chan motionEvent) error {
	// TODO: Instead of generating m3u8 files, create MP4 files using -v:c copy.
	// It will be performant and much easier to manage! This enables us to keep X
	// last days of full recording as .ts files and motion for Y last days as
	// .mp4, where Y is significantly larger than X.
	const reprocess = time.Minute
	var toGen [][3]time.Time
	var last time.Time
	var retryGen <-chan time.Time
	done := ctx.Done()
loop:
	for {
		select {
		case n := <-retryGen:
			for len(toGen) != 0 {
				if l := toGen[0]; n.After(l[2]) {
					// Best effort.
					if err := generateM3U8(root, l[0], l[1], l[2]); err != nil {
						return err
					}
					toGen = toGen[1:]
				}
			}
			if len(toGen) != 0 {
				retryGen = time.After(reprocess)
			}
		case <-done:
			break loop
		case event, ok := <-ch:
			if !ok {
				break loop
			}
			slog.Info("motionEvent", "t", event.t.Format("2006-01-02T15:04:05.00"), "start", event.start)
			if event.start {
				// Create a simple m3u8 file. Will be populated later.
				last = event.t
			}
			start := last.Add(-mo.preCapture)
			end := event.t.Add(reprocess + mo.postCapture)
			if err := generateM3U8(root, last, start, end); err != nil {
				return err
			}
			if !event.start {
				toGen = append(toGen, [...]time.Time{event.t, start, end})
				retryGen = time.After(reprocess)
			}
			if event.start {
				if mo.onEventStart != "" {
					if err := runCmd(ctx, mo.onEventStart); err != nil {
						slog.Error("on_event_start", "p", mo.onEventStart, "err", err)
					}
				}
			} else {
				if mo.onEventEnd != "" {
					if err := runCmd(ctx, mo.onEventEnd); err != nil {
						slog.Error("on_event_end", "p", mo.onEventEnd, "err", err)
					}
				}
			}
			if mo.webhook != "" {
				d, _ := json.Marshal(map[string]bool{"motion": event.start})
				slog.Info("webhook", "url", mo.webhook, "motion", event.start)
				// #nosec G107
				resp, err := http.Post(mo.webhook, "application/json", bytes.NewReader(d))
				if err != nil {
					slog.Error("webhook", "url", mo.webhook, "motion", event.start, "err", err)
				} else {
					_ = resp.Body.Close()
				}
			}
		}
	}
	for _, l := range toGen {
		if err := generateM3U8(root, l[0], l[1], l[2]); err != nil {
			return err
		}
	}
	return nil
}
