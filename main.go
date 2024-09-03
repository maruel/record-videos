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
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"golang.org/x/sync/errgroup"
)

// run is the main loop.
func run(ctx context.Context, root, addr string, fo *ffmpegOptions, mo *motionOptions) error {
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
	defer func() {
		if err2 := metadataR.Close(); err2 != nil {
			slog.Error("metadataR", "err", err2)
		}
	}()
	mpjpegR, mpjpegW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() {
		if err2 := mpjpegR.Close(); err2 != nil {
			slog.Error("mpjpegR", "err", err2)
		}
	}()
	defer func() {
		if err2 := mpjpegW.Close(); err2 != nil {
			slog.Error("mpjpegW", "err", err2)
		}
	}()
	args, err := buildFFMPEGCmd(fo)
	if err != nil {
		if err2 := metadataW.Close(); err2 != nil {
			slog.Error("metadataW", "err", err2)
		}
		return err
	}
	eg, ctx := errgroup.WithContext(ctx)
	if addr != "" {
		if err = startServer(ctx, addr, mpjpegR, root); err != nil {
			if err2 := metadataW.Close(); err2 != nil {
				slog.Error("metadataW", "err", err2)
			}
			return err
		}
	}

	start := time.Now().Round(10 * time.Millisecond)
	ch := make(chan yLevel, 10)
	events := make(chan motionEvent, 10)
	eg.Go(func() error {
		defer close(ch)
		err2 := processMetadata(start, metadataR, ch)
		slog.Info("processMetadata", "msg", "exit", "err", err2)
		return err2
	})
	eg.Go(func() error {
		defer close(events)
		err2 := filterMotion(ctx, mo, start, ch, events)
		slog.Info("filterMotion", "msg", "exit", "err", err2)
		return err2
	})
	eg.Go(func() error {
		err2 := processMotion(ctx, mo, root, events)
		slog.Info("processMotion", "msg", "exit", "err", err2)
		return err2
	})
	eg.Go(func() error {
		// TODO: Transparently restart ffmpeg when network or USB goes down as long as
		// the context is not canceled.
		// One challenge is when the TCP stream stops, it's the keep-alive that
		// detects that ffmpeg needs to be restarted, so the processMetadata should
		// be associated with the code here.
		// TODO: Does this requires us to get rid of start?

		// This is necessary because processMetadata doesn't accept a context.
		defer func() {
			if err2 := metadataW.Close(); err2 != nil {
				slog.Error("metadataW", "err", err2)
			}
		}()
		//for ctx.Err() == nil {
		// If any of the eg.Go() call above returns an error, this will kill ffmpeg
		// via ctx.
		cmd := cmdFFMPEG(ctx, root, args, []*os.File{metadataW, mpjpegW})
		if err2 := cmd.Start(); err2 != nil {
			return err2
		}
		// ffmpeg always return an error, so ignore it.
		err2 := cmd.Wait()
		slog.Info("ffmpeg", "msg", "exit", "err", err2)
		//}
		return nil
	})
	return eg.Wait()
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
	src := flag.String("src", "", "source to use: either a local device or a remote port, see README.md for more information")
	mask := flag.String("mask", "", "image mask to use; white means area to detect. Automatically resized to frame size")
	w := flag.Int("w", 1280, "width")
	h := flag.Int("h", 720, "height")
	fps := flag.Int("fps", 15, "frame rate")
	d := flag.Duration("d", 0, "record for a specified duration (for testing)")
	s := validStyles[0]
	flag.Var(&s, "style", "style to use")
	codec := flag.String("codec", "h264", "codec to use; libx265 takes significantly more CPU")
	yavg := flag.Float64("yavg", 1., "Y average sensitivity, higher value means lower sensitivity")
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
	defer func() {
		if err2 := wat.Close(); err2 != nil {
			slog.Error("watcher", "err", err2)
		}
	}()
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
	if *src == "" {
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
		case "windows":
			c := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-f", "dshow", "-list_devices", "true", "-i", "")
			out, _ = c.CombinedOutput()
		default:
			return fmt.Errorf("-src not specified")
		}
		return fmt.Errorf("-src not specified, here's what has been found:\n\n%s", bytes.TrimSpace(out))
	}
	fo := &ffmpegOptions{
		src:   *src,
		mask:  *mask,
		w:     *w,
		h:     *h,
		fps:   *fps,
		d:     *d,
		s:     s,
		codec: *codec,
		// Enable mpjpeg encoding only if the server is running.
		mpjpeg:  *addr != "",
		verbose: *verbose,
	}
	mo := &motionOptions{
		yThreshold:         float32(*yavg),
		motionExpiration:   5 * time.Second,
		preCapture:         5 * time.Second,
		postCapture:        2 * time.Second,
		ignoreFirstFrames:  10,
		ignoreFirstMoments: 5 * time.Second,
		onEventStart:       *onEventStart,
		onEventEnd:         *onEventEnd,
		webhook:            *webhook,
	}
	return run(ctx, *root, *addr, fo, mo)
}

func main() {
	if err := mainImpl(); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "record-videos: %s\n", err.Error())
		os.Exit(1)
	}
}
