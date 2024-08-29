// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// record-videos records videos to a directory.
//
// Should be paired with serve-videos.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"golang.org/x/sync/errgroup"
)

func getWd() string {
	wd, _ := os.Getwd()
	return wd
}

// createFilter constructs the argument for -filter_complex.
func createFilter(now time.Time) string {
	// References:
	// - https://ffmpeg.org/ffmpeg-filters.html

	// TODO: figure out why I can't use hqdn3d once, something like
	// "[0:v]hqdn3d[src];" then use [src] from there on.

	// Start by doing edge detection.
	filter := "[0:v]"
	// Speed up (? To be confirmed) edge detection by reducing the image by 4x.
	filter += "scale=w=iw/2:h=ih/2,"
	// Testing: filter += "[1:v]alphamerge,overlay,"
	// Reduce noise.
	filter += "hqdn3d,"
	// Manual edge detection with a threshold:
	//	filter := "edgedetect,tblend=all_expr='max(abs(A-B)-0.75,0)*4'"
	filter += "tblend=all_mode=difference,edgedetect,"

	filter += "signalstats,"
	// Debugging: Draw the YAVG on the image:
	//	filter += ",drawtext=" +
	//		"fontfile=/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf:" +
	//		"text='%{metadata\\:lavfi.signalstats.YAVG}':" +
	//		"x=10:" +
	//		"y=10:" +
	//		"fontsize=48:" +
	//		"fontcolor=white:" +
	//		"box=1:" +
	//		"boxcolor=black@0.5:"

	// Select frames where YAVG is high enough. The disadvantage is that the
	// frame number becomes off.
	filter += "metadata=mode=select:key=lavfi.signalstats.YAVG:value=0.1:function=greater,"
	filter += "metadata=print:key=lavfi.signalstats.YAVG:file='pipe\\:3':direct=1,"

	filter += "trim=end=1,nullsink"
	//filter += "[v1]"

	// Debugging: Blend both video streams:
	filter += "; [0:v]"
	//filter += "[v1]blend=all_expr='if(gte(B, 0.5),B,A)',"
	filter += "hqdn3d"

	// Draw the current time as an overlay.
	filter += ",drawtext=" +
		"fontfile=/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf:" +
		"text='%{pts\\:localtime\\:" + strconv.FormatInt(now.Unix(), 10) + "\\:%Y-%m-%d %T}':" +
		"x=(w-text_w-10):" +
		"y=(h-text_h-10):" +
		"fontsize=48:" +
		"fontcolor=white:" +
		"box=1:" +
		"boxcolor=black@0.5"
	filter += "[out]"
	return filter
}

func run(ctx context.Context, cam string, w, h, fps int, mask, root string) error {
	// References:
	// - https://ffmpeg.org/ffmpeg-all.html
	// - https://ffmpeg.org/ffmpeg-codecs.html
	// - https://ffmpeg.org/ffmpeg-formats.html
	// - https://ffmpeg.org/ffmpeg-utils.html
	// - https://trac.ffmpeg.org/wiki/Capture/Webcam
	//   ffmpeg -hide_banner -f v4l2 -list_formats all -i /dev/video3
	// - https://trac.ffmpeg.org/wiki/Encode/H.264
	secsPerSegment := 4
	size := strconv.Itoa(w) + "x" + strconv.Itoa(h)
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()

	start := time.Now()

	// #nosec G204
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-fflags", "nobuffer",
		"-analyzeduration", "0",
		// Testing:
		"-f", "v4l2",
		"-video_size", size,
		"-framerate", strconv.Itoa(fps),
		"-t", "00:00:05",
		"-i", cam,
		//"-i", mask,
		//"-f", "lavfi",
		//"-i", "color=c=black:s="+size,
		"-filter_complex", createFilter(start),
		"-map", "[out]",

		// h264
		"-c:v", "libx264",
		"-x264opts", "keyint="+strconv.Itoa(fps*secsPerSegment)+":min-keyint="+strconv.Itoa(fps*secsPerSegment)+":no-scenecut",
		"-preset", "fast",

		// MP4:
		//	"-movflags", "+faststart",
		//	"foo.mp4",

		// Sequence of images:
		//	"-qscale:v", "2",
		//	"output_frames_%04d.jpg",

		// HLS
		"-f", "hls",
		"-hls_time", strconv.Itoa(secsPerSegment),
		"-hls_list_size", "5",
		"-strftime", "1",
		"-hls_allow_cache", "1",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", "%Y-%m-%dT%H-%M-%S.ts",
		"output_playlist.m3u8",
	)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{pw}
	if err = cmd.Start(); err != nil {
		_ = pw.Close()
		return err
	}
	eg := errgroup.Group{}
	eg.Go(func() error {
		b := bufio.NewScanner(pr)
		// Form:
		//	frame:0    pts:87991   pts_time:0.087991
		//	lavfi.signalstats.YAVG=0.213281
		frame := 0
		var ptsTime time.Duration
		yavg := 0.
		var err2 error
		for b.Scan() {
			l := b.Text()
			// fmt.Printf("%s\n", l)
			if a, ok := strings.CutPrefix(l, "lavfi.signalstats.YAVG="); ok {
				if yavg, err2 = strconv.ParseFloat(a, 64); err2 != nil {
					slog.Error("metadata", "err", err2)
					return fmt.Errorf("unexpected metadata output: %q", l)
				}
				//fmt.Printf("(#% 4d) %-10s %.1f\n", frame, ptsTime.Round(10*time.Millisecond), yavg)
				//fmt.Printf("%-10s %.1f\n", ptsTime.Round(10*time.Millisecond), yavg)
				fmt.Printf("%s %.1f\n", start.Add(ptsTime).Format("2006-01-02T15:04:05.00"), yavg)
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
	})
	err = cmd.Wait()
	// Guarantee the pipe is closed in case of error.
	_ = pw.Close()
	if err2 := eg.Wait(); err == nil {
		err = err2
	}
	return err
}

func mainImpl() error {
	logger := slog.New(tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
		Level:      slog.LevelDebug,
		TimeFormat: time.TimeOnly,
		NoColor:    !isatty.IsTerminal(os.Stderr.Fd()),
	}))
	slog.SetDefault(logger)
	cam := flag.String("camera", "", "camera to use")
	w := flag.Int("w", 640, "width")
	h := flag.Int("h", 480, "height")
	fps := flag.Int("fps", 15, "frame rate")
	mask := flag.String("mask", "", "mask to use")
	root := flag.String("root", getWd(), "root directory")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if flag.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	if *cam == "" {
		c := exec.CommandContext(ctx, "v4l2-ctl", "--list-devices")
		out, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("fail to run v4l2-ctl, try 'sudo apt install v4l-utils'? %w", err)
		}
		// TODO gather resolutions: v4l2-ctl --list-formats-ext -d (dev)
		return fmt.Errorf("-camera not specified, here's what has been found:\n\n%s", bytes.TrimSpace(out))
	}
	tmp := ""
	if *mask == "" {
		f, err := os.CreateTemp("", "record-video-mask*.png")
		if err != nil {
			return err
		}
		// TODO:
		// *mask = "color=c=white:s=640x400"
		tmp = f.Name()
		slog.Info("mask", "name", tmp)
		img := image.NewRGBA(image.Rect(0, 0, *w, *h))
		// Testing:
		white := color.RGBA{255, 255, 255, 255}
		black := color.RGBA{0, 0, 0, 255}
		for x := 0; x < *w; x++ {
			for y := 0; y < *h; y++ {
				img.Set(x, y, black)
			}
		}
		for x := 0; x < 10; x++ {
			for y := 0; y < *h; y++ {
				img.Set(x, y, white)
			}
		}
		if err = png.Encode(f, img); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err = f.Close(); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		*mask = tmp
	}
	err := run(ctx, *cam, *w, *h, *fps, *mask, *root)
	if tmp != "" {
		if err2 := os.Remove(tmp); err == nil {
			err = err2
		}
	}
	return err
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "record-videos: %s\n", err.Error())
		os.Exit(1)
	}
}
