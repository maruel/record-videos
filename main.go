// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// record-videos records videos to a directory.
//
// Should be paired with serve-videos.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"golang.org/x/sync/errgroup"
)

// run is the main loop.
//
// TODO: transparently restart ffmpeg as needed, instead of exiting the whole
// program.
func run(ctx context.Context, src, style string, d time.Duration, w, h, fps int, yavg float64, mask, root, addr, onEventStart, onEventEnd, webhook string) error {
	// References:
	// - https://ffmpeg.org/ffmpeg-all.html
	// - https://ffmpeg.org/ffmpeg-codecs.html
	// - https://ffmpeg.org/ffmpeg-formats.html
	// - https://ffmpeg.org/ffmpeg-utils.html
	// - https://trac.ffmpeg.org/wiki/Capture/Webcam
	//   ffmpeg -hide_banner -f v4l2 -list_formats all -i /dev/video3
	// - https://trac.ffmpeg.org/wiki/Encode/H.264
	metadataR, metadataW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer metadataR.Close()
	defer func() {
		if metadataW != nil {
			_ = metadataW.Close()
		}
	}()
	mjpegR, mjpegW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer mjpegR.Close()
	defer func() {
		if mjpegW != nil {
			_ = mjpegW.Close()
		}
	}()
	if addr != "" {
		// MJPEG encapsulated in multi-part MIME demuxer.
		// An option here would be to use ffmpeg's "-listen 1 http://0.0.0.0:port"
		// but I failed to make it work. Instead decode and reencode. It's a bit
		// wasteful but it should be fast enough.
		if err = startServer(ctx, addr, mjpegR, root); err != nil {
			return err
		}
	}
	size := strconv.Itoa(w) + "x" + strconv.Itoa(h)
	mjpeg := addr != ""
	verbose := slog.Default().Enabled(ctx, slog.LevelDebug)
	args, err := buildFFMPEGCmd(src, mask, size, fps, d, style, mjpeg, verbose)
	if err != nil {
		return err
	}
	eg, ctx := errgroup.WithContext(ctx)

	// TODO: These will outlive ffmpeg as ffmpeg is transparently restarted when
	// network or USB goes down.
	// TODO: Does this requires us to get rid of start?
	start := time.Now().Round(10 * time.Millisecond)
	ch := make(chan yLevel, 10)
	events := make(chan motionEvent, 10)
	eg.Go(func() error {
		defer close(ch)
		return processMetadata(start, metadataR, ch)
	})
	eg.Go(func() error {
		defer close(events)
		return filterMotion(ctx, start, yavg, ch, events)
	})
	eg.Go(func() error {
		return processMotion(ctx, root, events, onEventStart, onEventEnd, webhook)
	})

	// If any of the eg.Go() call below returns an error, this will kill ffmpeg
	// via ctx.
	// #nosec G204
	cmd := cmdFFMPEG(ctx, root, args, []*os.File{metadataW, mjpegW})
	if err = cmd.Start(); err != nil {
		return nil
	}
	// TODO: do not close the write handles as we will reuse them when restarting
	// ffmpeg. This is so the server can keep the read handles untouched.
	_ = metadataW.Close()
	metadataW = nil
	_ = mjpegW.Close()
	mjpegW = nil

	err = cmd.Wait()
	if err2 := eg.Wait(); err == nil {
		err = err2
	}
	if ctx.Err() == context.Canceled {
		return nil
	}
	return err
}

type styleVar string

func (s *styleVar) Set(v string) error {
	switch v {
	case "normal", "normal_no_mask", "both", "motion_only":
		*s = styleVar(v)
		return nil
	default:
		return errors.New("invalid style. Supported values are: normal, normal_no_mask, both, motion_only")
	}
}

func (s *styleVar) String() string {
	return string(*s)
}

func mainImpl() error {
	var level slog.LevelVar
	level.Set(slog.LevelInfo)
	logger := slog.New(tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
		Level:      &level,
		TimeFormat: time.TimeOnly,
		NoColor:    !isatty.IsTerminal(os.Stderr.Fd()),
	}))
	slog.SetDefault(logger)
	cam := flag.String("camera", "", "camera to use")
	w := flag.Int("w", 1280, "width")
	h := flag.Int("h", 720, "height")
	fps := flag.Int("fps", 15, "frame rate")
	yavg := flag.Float64("yavg", 1., "Y average sensitivity, higher value means lower sensitivity")
	d := flag.Duration("d", 0, "record for a specified duration (for testing)")
	style := styleVar("normal")
	flag.Var(&style, "style", "style to use")
	mask := flag.String("mask", "", "image mask to use; white means area to detect. Automatically resized to frame size")
	root := flag.String("root", ".", "root directory to store videos into")
	addr := flag.String("addr", "", "optional address to listen to to serve MJPEG")
	onEventStart := flag.String("on-event-start", "", "script to run on motion event start")
	onEventEnd := flag.String("on-event-end", "", "script to run on motion event start")
	webhook := flag.String("webhook", "", "webhook to call on motion events")
	verbose := flag.Bool("v", false, "enable verbosity")
	flag.Parse()

	if flag.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	if *verbose {
		level.Set(slog.LevelDebug)
	}

	// Quit whenever SIGINT is received.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Quit whenever the executable is modified.
	e, err := os.Executable()
	if err != nil {
		return err
	}
	wat, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer wat.Close()
	if err = wat.Add(e); err != nil {
		return err
	}
	go func() {
		<-wat.Events
		cancel()
	}()

	if *root, err = filepath.Abs(filepath.Clean(*root)); err != nil {
		return err
	}
	if fi, err := os.Stat(*root); err != nil {
		return fmt.Errorf("-root %q is unusable: %w", *root, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("-root %q is not a directory", *root)
	}
	if *cam == "" {
		var out []byte
		var err error
		switch runtime.GOOS {
		case "darwin":
			c := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-f", "avfoundation", "-list_devices", "true", "-i", "")
			out, _ = c.CombinedOutput()
		case "linux":
			c := exec.CommandContext(ctx, "v4l2-ctl", "--list-devices")
			if out, err = c.CombinedOutput(); err != nil {
				return fmt.Errorf("fail to run v4l2-ctl, try 'sudo apt install v4l-utils'? %w", err)
			}
			// TODO gather resolutions too: v4l2-ctl --list-formats-ext -d (dev)
		default:
			return fmt.Errorf("-camera not specified")
		}
		return fmt.Errorf("-camera not specified, here's what has been found:\n\n%s", bytes.TrimSpace(out))
	}
	return run(ctx, *cam, style.String(), *d, *w, *h, *fps, *yavg, *mask, *root, *addr, *onEventStart, *onEventEnd, *webhook)
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "record-videos: %s\n", err.Error())
		os.Exit(1)
	}
}
