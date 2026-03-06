// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// record-videos records videos to a directory.
//
// Should be paired with serve-videos.
//
// # Architecture
//
//	signal / fsnotify
//	      │
//	      ▼
//	mainImpl() ──► run()
//	                │
//	                ├─ startServer()        (HTTP, stays alive across restarts)
//	                │    └─ teeMimePart     (fan-out of MJPEG frames)
//	                │
//	                ├─ processMotion()      (goroutine, stays alive)
//	                │    └─ events chan
//	                │
//	                └─ restart loop
//	                     └─ runFFMPEGOnce() (per ffmpeg instance)
//	                          ├─ ffmpeg process
//	                          ├─ processMetadata()  (pipe fd 3 → yLevel)
//	                          ├─ filterMotion()     (yLevel → motionEvent)
//	                          └─ tm.listen()        (pipe fd 4 → MJPEG fan-out)
//
// # Data flows
//
//	metadataR/W  (os.Pipe)  ffmpeg fd 3  →  processMetadata
//	mpjpegR/W    (os.Pipe)  ffmpeg fd 4  →  teeMimePart.listen
//	ch           chan yLevel              processMetadata → filterMotion
//	events       chan motionEvent         filterMotion    → processMotion
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	slogmulti "github.com/samber/slog-multi"
	"golang.org/x/sync/errgroup"
)

func trimFloat64(groups []string, attr slog.Attr) slog.Attr {
	if attr.Value.Kind() == slog.KindFloat64 {
		return slog.String(attr.Key, fmt.Sprintf("%0.2f", attr.Value.Float64()))
		// return slog.Float64(attr.Key, math.Round(attr.Value.Float64()*10)*0.1)
	}
	return attr
}

// runFFMPEGOnce starts one ffmpeg instance alongside processMetadata and
// filterMotion. It returns when ffmpeg exits for any reason, including the
// keep-alive timeout. The events channel is never closed by this function.
//
// Pipe ownership: after cmd.Start() the parent closes its write ends
// (metadataW, mpjpegW). The child holds dups. When ffmpeg exits all write
// ends are gone, so the read ends return EOF and unblock processMetadata and
// teeMimePart.listen without any explicit signalling.
func runFFMPEGOnce(ctx context.Context, root string, args []string, ffmpegLog io.Writer, mo *motionOptions, events chan<- motionEvent, tm *teeMimePart, mpjpeg bool) error {
	metadataR, metadataW, err := os.Pipe()
	if err != nil {
		return err
	}
	handles := []*os.File{metadataW}
	var mpjpegR, mpjpegW *os.File
	if mpjpeg && tm != nil {
		if mpjpegR, mpjpegW, err = os.Pipe(); err != nil {
			_ = metadataR.Close()
			_ = metadataW.Close()
			return err
		}
		handles = append(handles, mpjpegW)
	}

	eg, ctx2 := errgroup.WithContext(ctx)
	cmd := cmdFFMPEG(ctx2, root, args, handles, ffmpegLog)
	if err = cmd.Start(); err != nil {
		_ = metadataR.Close()
		_ = metadataW.Close()
		if mpjpegR != nil {
			_ = mpjpegR.Close()
			_ = mpjpegW.Close()
		}
		return err
	}
	// Close parent's write ends; the child process holds its own dups.
	_ = metadataW.Close()
	if mpjpegW != nil {
		_ = mpjpegW.Close()
	}

	if mpjpegR != nil {
		go func() {
			defer func() { _ = mpjpegR.Close() }()
			err2 := tm.listen(ctx, mpjpegR, "ffmpeg")
			slog.Info("teeMimePart", "msg", "exit", "err", err2)
		}()
	}

	start := time.Now().Round(10 * time.Millisecond)
	ch := make(chan yLevel, 10)
	eg.Go(func() error {
		defer close(ch)
		defer func() { _ = metadataR.Close() }()
		err2 := processMetadata(start, metadataR, ch)
		slog.Info("processMetadata", "msg", "exit", "err", err2)
		return err2
	})
	eg.Go(func() error {
		err2 := filterMotion(ctx2, mo, start, ch, events)
		slog.Info("filterMotion", "msg", "exit", "err", err2)
		return err2
	})
	eg.Go(func() error {
		err2 := cmd.Wait()
		slog.Info("ffmpeg", "msg", "exit", "err", err2)
		return nil
	})
	return eg.Wait()
}

// run is the main loop.
//
// References:
//   - https://ffmpeg.org/ffmpeg-all.html
//   - https://ffmpeg.org/ffmpeg-codecs.html
//   - https://ffmpeg.org/ffmpeg-formats.html
//   - https://ffmpeg.org/ffmpeg-utils.html
//   - https://trac.ffmpeg.org/wiki/Capture/Webcam
//     ffmpeg -hide_banner -f v4l2 -list_formats all -i /dev/video3
//   - https://trac.ffmpeg.org/wiki/Encode/H.264
func run(ctx context.Context, root, addr string, fo *ffmpegOptions, ffmpegLog io.Writer, mo *motionOptions) error {
	args, err := buildFFMPEGCmd(fo)
	if err != nil {
		return err
	}
	var tm *teeMimePart
	if addr != "" {
		tm = &teeMimePart{}
		if err := startServer(ctx, addr, tm, root); err != nil {
			return err
		}
	}

	eg, ctx := errgroup.WithContext(ctx)
	events := make(chan motionEvent, 10)
	eg.Go(func() error {
		// Restart loop: restarts ffmpeg on any exit (stream loss, hang detected
		// by filterMotion keep-alive, buffer overrun, etc.).
		// Backoff: 1 s → 2 s → … → 30 s max; resets after a run ≥ 30 s.
		// Stops only when ctx is canceled (SIGINT or binary update via fsnotify).
		// processMotion and the events channel outlive individual ffmpeg runs.
		// TODO: all.m3u8 is overwritten on each restart; the new instance starts
		// a fresh playlist. Consider appending across restarts.
		defer close(events)
		const maxBackoff = 30 * time.Second
		const resetThreshold = 30 * time.Second
		backoff := time.Duration(0)
		for ctx.Err() == nil {
			t0 := time.Now()
			err2 := runFFMPEGOnce(ctx, root, args, ffmpegLog, mo, events, tm, fo.mpjpeg)
			if ctx.Err() != nil {
				return nil
			}
			if fo.d > 0 {
				// Duration-limited run (testing); do not restart.
				return err2
			}
			slog.Warn("ffmpeg", "err", err2, "restart", true)
			if time.Since(t0) >= resetThreshold {
				backoff = 0
			}
			if backoff == 0 {
				backoff = time.Second
			} else {
				backoff = min(backoff*2, maxBackoff)
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
		}
		return nil
	})
	eg.Go(func() error {
		err2 := processMotion(ctx, mo, root, events)
		slog.Info("processMotion", "msg", "exit", "err", err2)
		return err2
	})
	return eg.Wait()
}

func mainImpl() error {
	var level slog.LevelVar
	level.Set(slog.LevelInfo)
	hldr := tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
		Level:       &level,
		TimeFormat:  time.TimeOnly,
		NoColor:     !isatty.IsTerminal(os.Stderr.Fd()),
		ReplaceAttr: trimFloat64,
	})
	slog.SetDefault(slog.New(hldr))
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
	addr := flag.String("addr", "", "optional address to listen to serve MJPEG")
	onEventStart := flag.String("on-event-start", "", "script to run on motion event start")
	onEventEnd := flag.String("on-event-end", "", "script to run on motion event start")
	webhook := flag.String("webhook", "", "webhook to call on motion events")
	verbose := flag.Bool("v", false, "enable verbosity")
	l := flag.String("logdir", "", "directory to log files to; reduces output to stderr")
	flag.Parse()

	if flag.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	ffmpegLevel := "repeat+warning"
	if *verbose {
		level.Set(slog.LevelDebug)
		ffmpegLevel = "repeat+info"
	}
	ffmpegLog := os.Stderr
	if *l != "" {
		l2, err := filepath.Abs(*l)
		if err != nil {
			return fmt.Errorf("-l: %w", err)
		}
		// #nosec G302 G304
		f, err := os.OpenFile(filepath.Join(l2, "recd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer func() {
			if err2 := f.Close(); err2 != nil {
				slog.Error("f.Close", "err", err2)
			}
		}()
		// Revert back log to warning.
		level.Set(slog.LevelWarn)
		hldr2 := tint.NewHandler(f, &tint.Options{
			Level:       slog.LevelDebug,
			TimeFormat:  time.TimeOnly,
			NoColor:     true,
			ReplaceAttr: trimFloat64,
		})
		slog.SetDefault(slog.New(slogmulti.Fanout(hldr, hldr2)))
		// #nosec G302 G304
		ffmpegLog, err = os.OpenFile(filepath.Join(l2, "ffmpeg.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer func() {
			if err2 := ffmpegLog.Close(); err2 != nil {
				slog.Error("ffmpegLog.Close", "err", err2)
			}
		}()
		ffmpegLevel = "repeat+level+verbose"
		if *verbose {
			ffmpegLevel = "repeat+level+debug"
		}
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
	if err := wat.Add(e); err != nil {
		return err
	}
	go func() {
		<-wat.Events
		cancel()
	}()

	if *root, err = filepath.Abs(*root); err != nil {
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
			return errors.New("-src not specified")
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
		mpjpeg: *addr != "",
		level:  ffmpegLevel,
	}
	mo := &motionOptions{
		yThreshold:         float32(*yavg),
		motionExpiration:   5 * time.Second,
		preCapture:         5 * time.Second,
		postCapture:        2 * time.Second,
		ignoreFirstFrames:  10,
		ignoreFirstMoments: 5 * time.Second,
		keepAlive:          10 * time.Second,
		onEventStart:       *onEventStart,
		onEventEnd:         *onEventEnd,
		webhook:            *webhook,
	}
	return run(ctx, *root, *addr, fo, ffmpegLog, mo)
}

func main() {
	if err := mainImpl(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "record-videos: %s\n", err.Error())
		os.Exit(1)
	}
}
