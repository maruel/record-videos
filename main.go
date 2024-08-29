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

// filter is a filter supported by libavfilter.
//
// They are described at https://ffmpeg.org/ffmpeg-filters.html.
type filter string

func (f filter) String() string {
	return string(f)
}

// chain is a list of filters that are piped together.
type chain []filter

func (c chain) String() string {
	out := ""
	for i, f := range c {
		if i != 0 {
			out += ","
		}
		out += f.String()
	}
	return out
}

// buildChain builds a filter chain from various filters and preexisting
// chains.
func buildChain(next ...any) chain {
	var out chain
	for _, i := range next {
		switch t := i.(type) {
		case filter:
			out = append(out, t)
		case string:
			// Warning: error prone. It's included so we don't have to create a
			// constant for every single filter (there's a lot!).
			out = append(out, filter(t))
		case chain:
			out = append(out, t...)
		default:
			panic("internal error")
		}
	}
	return out
}

// drawTimestamp draws the current timestamp as an overlay.
const drawTimestamp filter = "drawtext=" +
	"fontfile=/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf:" +
	"text='%{localtime\\:%Y-%m-%d %T}':" +
	"x=(w-text_w-10):" +
	"y=(h-text_h-10):" +
	"fontsize=48:" +
	"fontcolor=white:" +
	"box=1:" +
	"boxcolor=black@0.5"

// drawYAVG draws the YAVG on the image for debugging. Requires signalstats.
//
// TODO: Figure out how to round the number printed out.
const drawYAVG filter = "drawtext=" +
	"fontfile=/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf:" +
	"text='%{metadata\\:lavfi.signalstats.YAVG}':" +
	"x=10:" +
	"y=10:" +
	"fontsize=48:" +
	"fontcolor=white:" +
	"box=1:" +
	"boxcolor=black@0.5"

// scaleHalf reduces the image by half on both dimensions, to reduce the
// processing power required by 75%.
const scaleHalf filter = "scale=w=iw/2:h=ih/2"

// motionEdgeDetect does motion detection by calculating the edges on the delta
// between each images.
var motionEdgeDetect = chain{"tblend=all_mode=difference", "edgedetect"}

var motionEdgeDetectManual = chain{"tblend=all_expr='abs(A-B)'", "edgedetect"}

// edgeMotionDetect calculate the deltas between the edges. Uses a minimum
// threshold.
//
// Doesn't seem to be a good idea in practice.
var edgeMotionDetect = chain{"edgedetect", "tblend=all_expr='max(abs(A-B)-0.75,0)*4'"}

// discardLowYAVGFrames discards frames with YAVG value below 0.1.
//
// The disadvantage is that the frame number becomes off.
const discardLowYAVGFrames filter = "metadata=mode=select:key=lavfi.signalstats.YAVG:value=0.1:function=greater"

// printYAVGtoPipe prints YAVG to pipe #3. This is the first pipe specified in
// exec.Cmd.ExtraFiles.
const printYAVGtoPipe filter = "metadata=print:key=lavfi.signalstats.YAVG:file='pipe\\:3':direct=1"

// discard discards the stream. Useful if only statistics (e.g. YAVG) are
// necessary and not the stream's pixels.
var discard = chain{"trim=end=1", "nullsink"}

// stream is a stream that takes an input, passes it through a chain of filters
// and sink into the output.
type stream struct {
	// sources are optional input streams like "0:v".
	sources []string
	chain   chain
	// sinks are optional output streams like "tmp".
	sinks []string
}

func (s *stream) String() string {
	out := ""
	for _, src := range s.sources {
		//out += "[" + src + "]"
		out += src
	}
	out += s.chain.String()
	for _, dst := range s.sinks {
		//out += "[" + dst + "]"
		out += dst
	}
	return out
}

// filterGraph is a series of stream to pass to ffmpeg.
type filterGraph []stream

func (f filterGraph) String() string {
	out := ""
	for i, g := range f {
		if i != 0 {
			out += ";"
		}
		out += g.String()
	}
	return out
}

// constructFilterGraph constructs the argument for -filter_complex.
func constructFilterGraph(verbose bool, size string) string {
	// TODO: figure out why I can't use hqdn3d once, something like
	// "[0:v]hqdn3d[src];" then use [src] from there on.

	if verbose {
		// TODO: No idea why YAVG floor becomes 16 instead of 0 here.
		f := filterGraph{
			{
				sources: []string{"[0:v]"},
				chain:   buildChain("hqdn3d", "split=2"),
				sinks:   []string{"[v0]", "[v1]"},
			},
			{
				sources: []string{"[v0][1:v]"},
				chain:   buildChain("alphamerge"),
				sinks:   []string{"[alpha]"},
			},
			{
				chain: buildChain("color=color=black:size=" + size),
				sinks: []string{"[black]"},
			},
			{
				sources: []string{"[black][alpha]"},
				chain:   buildChain("overlay"),
				sinks:   []string{"[masked]"},
			},
			{
				sources: []string{"[masked]"},
				chain:   buildChain(motionEdgeDetect, "signalstats", printYAVGtoPipe, drawYAVG, "format=yuv420p"),
				sinks:   []string{"[motion]"},
			},
			{
				sources: []string{"[v1]", "[motion]"},
				//chain:   chain{"blend=all_expr='if(gte(B, 0.5),B,A)'", drawTimestamp},
				chain: chain{"blend=all_mode='lighten'", drawTimestamp},
				sinks: []string{"[out]"},
			},
		}
		return f.String()
	}

	f := filterGraph{
		{
			sources: []string{"[0:v]"},
			chain:   buildChain("hqdn3d", "split=2"),
			sinks:   []string{"[v0]", "[v1]"},
		},
		{
			sources: []string{"[v0][1:v]"},
			chain:   buildChain("alphamerge"),
			sinks:   []string{"[alpha]"},
		},
		{
			chain: buildChain("color=color=black:size=" + size),
			sinks: []string{"[black]"},
		},
		{
			sources: []string{"[black][alpha]"},
			chain:   buildChain("overlay"),
			sinks:   []string{"[masked]"},
		},
		{
			sources: []string{"[masked]"},
			// Speed up (? To be confirmed) edge detection by reducing the image by 4x
			// and reduce noise: scaleHalf, "hqdn3d"
			chain: buildChain(
				motionEdgeDetect, "signalstats", discardLowYAVGFrames, printYAVGtoPipe, discard),
		},
		{
			sources: []string{"[v1]"},
			chain:   chain{drawTimestamp},
			sinks:   []string{"[out]"},
		},
	}
	return f.String()
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

	args := []string{
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-fflags", "nobuffer",
		"-analyzeduration", "0",

		"-f", "v4l2",
		"-video_size", size,
		"-framerate", strconv.Itoa(fps),
		"-i", cam,
		"-i", mask,

		"-filter_complex", constructFilterGraph(false, size),
		"-map", "[out]",

		// Testing:
		"-t", "00:00:10",
	}

	// Codec (h264):
	args = append(args,
		"-c:v", "libx264",
		"-x264opts", "keyint="+strconv.Itoa(fps*secsPerSegment)+":min-keyint="+strconv.Itoa(fps*secsPerSegment)+":no-scenecut",
		"-preset", "fast",
	)
	// Sequence of images (don't forget to disable h264)
	if false {
		args = append(args, "-qscale:v", "2", "output_frames_%04d.jpg")
	}
	// MP4
	if false {
		args = append(args, "-movflags", "+faststart", "foo.mp4")
	}
	// HLS
	if true {
		args = append(args,
			"-f", "hls",
			"-hls_time", strconv.Itoa(secsPerSegment),
			"-hls_list_size", "5",
			"-strftime", "1",
			"-hls_allow_cache", "1",
			"-hls_flags", "independent_segments",
			"-hls_segment_filename", "%Y-%m-%dT%H-%M-%S.ts",
			"output_playlist.m3u8",
		)
	}

	slog.Debug("running", "cmd", args)
	start := time.Now()
	// #nosec G204
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
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
			slog.Debug("metadata", "l", l)
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
	var level slog.LevelVar
	level.Set(slog.LevelInfo)
	logger := slog.New(tint.NewHandler(colorable.NewColorable(os.Stderr), &tint.Options{
		Level:      &level,
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
	verbose := flag.Bool("v", false, "verbose")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if flag.NArg() != 0 {
		return errors.New("unexpected argument")
	}
	if *verbose {
		level.Set(slog.LevelDebug)
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
		// TODO:
		if false {
			*mask = "color=color=white:size=" + strconv.Itoa(*w) + "x" + strconv.Itoa(*h)
		}
		f, err := os.CreateTemp("", "record-video-mask*.png")
		if err != nil {
			return err
		}
		tmp = f.Name()
		slog.Debug("mask", "path", tmp)
		img := image.NewRGBA(image.Rect(0, 0, *w, *h))
		white := color.RGBA{255, 255, 255, 255}
		for x := 0; x < *w; x++ {
			for y := 0; y < *h; y++ {
				img.Set(x, y, white)
			}
		}
		if false {
			// Testing: use a partial mask.
			for x := 100; x < *w; x++ {
				for y := 0; y < *h; y++ {
					img.Set(x, y, color.RGBA{})
				}
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
