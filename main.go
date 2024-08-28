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
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
)

func getWd() string {
	wd, _ := os.Getwd()
	return wd
}

func run(ctx context.Context, cam string, w, h, fps int, mask, root string) error {
	// References:
	// - https://trac.ffmpeg.org/wiki/Capture/Webcam
	//   ffmpeg -hide_banner -f v4l2 -list_formats all -i /dev/video3
	// - https://ffmpeg.org/ffmpeg-filters.html#blend
	// - https://ffmpeg.org/ffmpeg-utils.html#toc-Expression-Evaluation

	// Draw text as an overlay.
	epoch := strconv.FormatInt(time.Now().Unix(), 10)
	fontfile := "/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf"
	content := "text='%{pts\\:gmtime\\:" + epoch + "\\:%Y-%m-%d %T}':"
	aspect := "fontsize=48: fontcolor=white: box=1: boxcolor=black@0.5:"
	location := "x=(w-text_w-10): y=(h-text_h-10):"
	drawtext := "drawtext=fontfile=" + fontfile + ":" + content + location + aspect

	// Do edge detection as a manual alternative to "edgedetect,tblend=all_mode=difference".
	edgedetect := "edgedetect,tblend=all_expr='max(abs(A-B)-0.75,0)*4'"

	// Blend both video streams.
	blend := "blend=all_expr='if(gte(B, 0.5),B,A)'"

	// Next:
	// "scale=iw/2:ih/2,"
	// metadata=mode=print:file='pipe\:4'

	// The final filter:
	filter := "[0:v]" + edgedetect + "[v1]; [0:v][v1]" + blend + "," + drawtext + "[out]"

	// #nosec G204
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner",
		"-f", "v4l2",
		"-video_size", strconv.Itoa(w)+"x"+strconv.Itoa(h),
		"-framerate", strconv.Itoa(fps),
		"-i", cam,
		// Testing:
		"-t", "00:00:5",

		//"-i", mask,

		// Create a stream of edge detection difference.
		"-filter_complex", filter,
		"-map", "[out]",

		// MP4:
		"-c:v", "libx264",
		"-preset", "fast",
		"-movflags", "+faststart",
		"foo.mp4",

		/*
			"-c:v", "libx264",
			"-preset", "fast",
			"-f", "hls",
			"-hls_time", "4",
			"-hls_list_size", "0",
			"-hls_segment_filename", "segment_%03d.ts",
			"output_playlist.m3u8",
		*/
	)
	/*
		// Sequence of images:
		"-qscale:v", "2",
		"output_frames_%04d.jpg",
	*/
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
		img := image.NewRGBA(image.Rect(0, 0, *w, *h))
		white := color.RGBA{255, 255, 255, 255}
		for x := 0; x < *w; x++ {
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
