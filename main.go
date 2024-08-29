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
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"text/template"
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
// between each frame pairs.
var motionEdgeDetect = chain{
	// Do edge detection. This effectively half the frame rate.
	"tblend=all_mode=difference", "edgedetect",
	// Duplicate each frames and reset the frame time stamps.
	"tpad=stop_mode=clone:stop_duration=1", "setpts=N/FRAME_RATE/TB",
}

// edgeMotionDetect calculate the deltas between the edges. Uses a minimum
// threshold.
//
// Doesn't seem to be a good idea in practice.
var edgeMotionDetect = chain{"edgedetect", "tblend=all_expr='max(abs(A-B)-0.75,0)*4'"}

// printYAVGtoPipe prints YAVG to pipe #3 when the value is above 0.1.
//
// Pipe #3 is the first pipe specified in exec.Cmd.ExtraFiles.
const printYAVGtoPipe filter = "metadata=print:key=lavfi.signalstats.YAVG:function=greater:value=0.1:file='pipe\\:3':direct=1"

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
	return strings.Join(s.sources, "") + s.chain.String() + strings.Join(s.sinks, "")
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
func constructFilterGraph(style string, size string) string {
	// I could use scale2ref instead of manually specifying size for the black
	// mask buffer but I am guessing this will be significantly slower.
	var out filterGraph
	switch style {
	case "normal":
		out = filterGraph{
			{
				sources: []string{"[0:v]"},
				chain:   buildChain("hqdn3d", "split=2"),
				sinks:   []string{"[src1]", "[src2]"},
			},
			{
				sources: []string{"[1:v]"},
				chain:   buildChain("scale="+size, scaleHalf),
				sinks:   []string{"[mask]"},
			},
			{
				sources: []string{"[src1]"},
				chain:   buildChain(scaleHalf),
				sinks:   []string{"[srcHalf]"},
			},
			{
				sources: []string{"[srcHalf][mask]"},
				chain:   buildChain("alphamerge"),
				sinks:   []string{"[alpha]"},
			},
			{
				chain: buildChain("color=color=black:size="+size, scaleHalf),
				sinks: []string{"[black]"},
			},
			{
				sources: []string{"[black][alpha]"},
				chain:   buildChain("overlay"),
				sinks:   []string{"[masked]"},
			},
			{
				sources: []string{"[masked]"},
				chain:   buildChain(motionEdgeDetect, "signalstats", printYAVGtoPipe, "nullsink"),
			},
			{
				sources: []string{"[src2]"},
				chain:   buildChain(drawTimestamp),
				sinks:   []string{"[out]"},
			},
		}
	case "normal_no_mask":
		out = filterGraph{
			{
				sources: []string{"[0:v]"},
				chain:   buildChain("hqdn3d", "split=2"),
				sinks:   []string{"[src1]", "[src2]"},
			},
			{
				sources: []string{"[src1]"},
				chain:   buildChain(scaleHalf, motionEdgeDetect, "signalstats", printYAVGtoPipe, "nullsink"),
			},
			{
				sources: []string{"[src2]"},
				chain:   buildChain(drawTimestamp),
				sinks:   []string{"[out]"},
			},
		}
	case "motion_only":
		out = filterGraph{
			{
				sources: []string{"[0:v]"},
				chain:   buildChain("hqdn3d", scaleHalf),
				sinks:   []string{"[src]"},
			},
			{
				sources: []string{"[1:v]"},
				chain:   buildChain("scale="+size, scaleHalf, "split=2"),
				sinks:   []string{"[mask1]", "[mask2]"},
			},
			{
				sources: []string{"[src][mask1]"},
				chain:   buildChain("alphamerge"),
				sinks:   []string{"[alpha]"},
			},
			{
				chain: buildChain("color=color=black:size="+size, scaleHalf),
				sinks: []string{"[black]"},
			},
			{
				sources: []string{"[black][alpha]"},
				chain:   buildChain("overlay"),
				sinks:   []string{"[masked]"},
			},
			{
				sources: []string{"[masked]"},
				chain:   buildChain(motionEdgeDetect, "signalstats", printYAVGtoPipe),
				sinks:   []string{"[motion]"},
			},
			{
				chain: buildChain("color=color=red:size="+size, scaleHalf),
				sinks: []string{"[red]"},
			},
			{
				sources: []string{"[mask2]"},
				chain:   buildChain("lut=y=negval"),
				sinks:   []string{"[maskneg]"},
			},
			{
				sources: []string{"[red][maskneg]"},
				chain:   buildChain("alphamerge"),
				sinks:   []string{"[maskedred]"},
			},
			{
				sources: []string{"[motion][maskedred]"},
				chain:   buildChain("overlay"),
				sinks:   []string{"[out]"},
			},
		}
	case "both":
		out = filterGraph{
			{
				sources: []string{"[0:v]"},
				chain:   buildChain("hqdn3d", "split=2"),
				sinks:   []string{"[src1]", "[src2]"},
			},
			{
				sources: []string{"[1:v]"},
				chain:   buildChain("scale="+size, scaleHalf, "split=2"),
				sinks:   []string{"[mask1]", "[mask2]"},
			},
			{
				sources: []string{"[src1]"},
				chain:   buildChain(scaleHalf),
				sinks:   []string{"[srcHalf]"},
			},
			{
				sources: []string{"[srcHalf][mask1]"},
				chain:   buildChain("alphamerge"),
				sinks:   []string{"[alpha]"},
			},
			{
				chain: buildChain("color=color=black:size="+size, scaleHalf),
				sinks: []string{"[black]"},
			},
			{
				sources: []string{"[black][alpha]"},
				chain:   buildChain("overlay"),
				sinks:   []string{"[masked]"},
			},
			{
				sources: []string{"[masked]"},
				chain:   buildChain(motionEdgeDetect, "signalstats", printYAVGtoPipe, drawYAVG),
				sinks:   []string{"[motion]"},
			},
			{
				sources: []string{"[src2]"},
				chain:   buildChain(drawTimestamp, "pad='iw*1.5':ih"),
				sinks:   []string{"[out1]"},
			},
			{
				chain: buildChain("color=color=red:size="+size, scaleHalf),
				sinks: []string{"[red]"},
			},
			{
				sources: []string{"[mask2]"},
				chain:   buildChain("lut=y=negval"),
				sinks:   []string{"[maskneg]"},
			},
			{
				sources: []string{"[red][maskneg]"},
				chain:   buildChain("alphamerge"),
				sinks:   []string{"[maskedred]"},
			},
			{
				sources: []string{"[motion][maskedred]"},
				chain:   buildChain("overlay"),
				sinks:   []string{"[out2]"},
			},
			{
				sources: []string{"[out1][out2]"},
				chain:   buildChain("overlay='2*w'"),
				sinks:   []string{"[out]"},
			},
		}
	default:
		panic("unknown style " + style)
	}
	return out.String()
}

type motion struct {
	t    time.Time
	yavg float64
}

// processMetadata processes metadata from ffmpeg's metadata:print filter.
//
// It expects data in the form:
//
//	frame:1336 pts:1336    pts_time:53.44
//	lavfi.signalstats.YAVG=0.213281
func processMetadata(start time.Time, r io.Reader, ch chan<- motion) error {
	b := bufio.NewScanner(r)
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
			ch <- motion{t: start.Add(ptsTime), yavg: yavg}
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

var m3u8Tmpl = template.Must(template.New("").Parse(`#EXTM3U
#EXT-X-VERSION:6
#EXT-X-ALLOW-CACHE:YES
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-INDEPENDENT-SEGMENTS
{{range .}}#EXTINF:4.000000,
{{.}}
{{end}}
`))

func processMotion(root string, ch <-chan motion) error {
	// Determine if motion occurred.
	for event := range ch {
		slog.Info("motion", "t", event.t.Format("2006-01-02T15:04:05.00"), "yavg", event.yavg)
		// Look at the files written and creates a loop with the recent files.
		files := make([]string, 4)
		for i := 0; i < 4; i++ {
			files[i] = event.t.Format("2006-01-02T15-04-05") + ".ts"
		}
		f, err := os.Create(event.t.Format("2006-01-02T15-04-05") + ".m3u8")
		if err != nil {
			return err
		}
		if err = m3u8Tmpl.Execute(f, files); err != nil {
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func run(ctx context.Context, cam, style string, w, h, fps int, mask, root string) error {
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
	}
	switch runtime.GOOS {
	case "darwin":
		args = append(args, "-f", "avfoundation")
	case "linux":
		args = append(args, "-f", "v4l2")
	default:
		return errors.New("not implemented for this OS")
	}
	args = append(args,
		"-video_size", size,
		"-framerate", strconv.Itoa(fps),
		"-i", cam,
		"-i", mask,
		"-filter_complex", constructFilterGraph(style, size),
		"-map", "[out]",
	)
	if false {
		// Limit runtime for local testing.
		args = append(args, "-t", "00:00:05")
	}
	// Codec (h264):
	args = append(args,
		"-c:v", "libx264",
		"-x264opts", "keyint="+strconv.Itoa(fps*secsPerSegment)+":min-keyint="+strconv.Itoa(fps*secsPerSegment)+":no-scenecut",
		"-preset", "fast",
		"-crf", "30",
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
			"all.m3u8",
		)
	}

	ch := make(chan motion, 10)
	eg, ctx := errgroup.WithContext(ctx)
	slog.Debug("running", "cmd", args)
	// #nosec G204
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{pw}
	start := time.Now()
	if err = cmd.Start(); err == nil {
		eg.Go(func() error {
			defer close(ch)
			return processMetadata(start, pr, ch)
		})
		eg.Go(func() error { return processMotion(root, ch) })
		err = cmd.Wait()
	}
	_ = pw.Close()
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
	h := flag.Int("h", 768, "height")
	fps := flag.Int("fps", 15, "frame rate")
	style := styleVar("normal")
	flag.Var(&style, "style", "style to use")
	mask := flag.String("mask", "", "image mask to use; white means area to detect. Automatically resized to frame size")
	root := flag.String("root", getWd(), "root directory to store videos into")
	verbose := flag.Bool("v", false, "enable verbosity")
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
				img.SetRGBA(x, y, white)
			}
		}
		if false {
			// Testing: use a partial mask.
			black := color.RGBA{}
			for x := 0; x < *w; x++ {
				for y := 0; y < *h+(*w-*h)-x; y++ {
					img.SetRGBA(x, y, black)
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
	err := run(ctx, *cam, style.String(), *w, *h, *fps, *mask, *root)
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
