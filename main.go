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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"golang.org/x/sync/errgroup"
)

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

// printYAVGtoPipe prints YAVG to pipe #3 when the value is above 0.1.
//
// Pipe #3 is the first pipe specified in exec.Cmd.ExtraFiles.
const printYAVGtoPipe filter = "metadata=print:key=lavfi.signalstats.YAVG:file='pipe\\:3':direct=1"

// printFilteredYAVGtoPipe prints YAVG to pipe #3 when the value is above 0.1.
//
// Pipe #3 is the first pipe specified in exec.Cmd.ExtraFiles.
//
//lint:ignore U1000 not used because of keep-alive
const printFilteredYAVGtoPipe filter = "metadata=print:key=lavfi.signalstats.YAVG:function=greater:value=0.1:file='pipe\\:3':direct=1"

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
func constructFilterGraph(style string, size string) filterGraph {
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
				sinks:   []string{"[overlay1]"},
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
				sinks:   []string{"[overlay2]"},
			},
			{
				sources: []string{"[overlay1][overlay2]"},
				chain:   buildChain("overlay='2*w'"),
				sinks:   []string{"[out]"},
			},
		}
	default:
		panic("unknown style " + style)
	}
	return out
}

// motionLevel is the level of Y channel average on the image, which is the
// amount of edge movements detected.
type motionLevel struct {
	frame int
	t     time.Time
	yavg  float64
}

// motionEvent is a processed motionLevel to determine when motion started and
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
func processMetadata(start time.Time, r io.Reader, ch chan<- motionLevel) error {
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
			ch <- motionLevel{frame: frame, t: start.Add(ptsTime).Round(100 * time.Millisecond), yavg: yavg}
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
func filterMotion(ctx context.Context, start time.Time, yavg float64, ch <-chan motionLevel, events chan<- motionEvent) error {
	// Eventually configuration values:
	const motionExpiration = 5 * time.Second
	// TODO: Get the ready signal from MPJPEG reader.
	// Many cameras will auto-focus and cause a lot of artificial motion when
	// starting up.
	const ignoreFirstFrames = 10
	const ignoreFirstMoments = 5 * time.Second
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
				slog.Info("motionLevel", "t", l.t.Format("2006-01-02T15:04:05.00"), "f", l.frame, "yavg", l.yavg)
			}
			if l.frame >= ignoreFirstFrames && l.t.Sub(start) >= ignoreFirstMoments && l.yavg >= yavg {
				motionTimeout = time.After(motionExpiration - time.Since(l.t))
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
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, a)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	slog.Info("cmd", "p", a)
	return c.Run()
}

// processMotion reacts to motion start and stop events.
func processMotion(ctx context.Context, root string, ch <-chan motionEvent, onEventStart, onEventEnd, webhook string) error {
	// libx264 can buffer 30s at a time.
	const lookBack = 31 * time.Second
	const reprocess = time.Minute
	var toGen [][3]time.Time
	var last time.Time
	var retryGen <-chan time.Time
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
		case event, ok := <-ch:
			if !ok {
				for _, l := range toGen {
					if err := generateM3U8(root, l[0], l[1], l[2]); err != nil {
						return err
					}
				}
				return nil
			}
			slog.Info("motionEvent", "t", event.t.Format("2006-01-02T15:04:05.00"), "start", event.start)
			if event.start {
				// Create a simple m3u8 file. Will be populated later.
				last = event.t
			}
			start := last.Add(-lookBack)
			end := event.t.Add(reprocess)
			if err := generateM3U8(root, last, start, end); err != nil {
				return err
			}
			if !event.start {
				toGen = append(toGen, [...]time.Time{event.t, start, end})
				retryGen = time.After(reprocess)
			}
			if event.start {
				if onEventStart != "" {
					if err := runCmd(ctx, onEventStart); err != nil {
						slog.Error("on_event_start", "p", onEventStart, "err", err)
					}
				}
			} else {
				if onEventEnd != "" {
					if err := runCmd(ctx, onEventEnd); err != nil {
						slog.Error("on_event_end", "p", onEventEnd, "err", err)
					}
				}
			}
			if webhook != "" {
				d, _ := json.Marshal(map[string]bool{"motion": event.start})
				slog.Info("webhook", "url", webhook, "motion", event.start)
				// #nosec G107
				resp, err := http.Post(webhook, "application/json", bytes.NewReader(d))
				if err != nil {
					slog.Error("webhook", "url", webhook, "motion", event.start, "err", err)
				} else {
					_ = resp.Body.Close()
				}
			}
		}
	}
}

// buildFFMPEGCmd builds the command line to exec ffmpeg.
func buildFFMPEGCmd(src, mask, size string, fps int, d time.Duration, style string, mjpeg, verbose bool) ([]string, error) {
	args := []string{
		"ffmpeg",
		"-hide_banner",
		// Disable stats output because it uses CR character, which corrupts logs.
		"-nostats",
		// Enable automatic hardware acceleration for encoding. This can fail in
		// weird ways, like trying to load CUDA when there's no nvidia hardware
		// present.
		//"-hwaccel", "auto",
	}
	if verbose {
		args = append(args, "-loglevel", "repeat+info")
	} else {
		args = append(args, "-loglevel", "repeat+warning")
	}
	if strings.HasPrefix(src, "tcp://") {
		args = append(args, "-f", "h264")
	} else {
		switch runtime.GOOS {
		case "darwin":
			args = append(args, "-f", "avfoundation")
		case "linux":
			args = append(args, "-f", "v4l2")
		default:
			return nil, errors.New("not implemented for this OS")
		}
		args = append(args,
			"-avioflags", "direct",
			"-fflags", "nobuffer",
			"-flags", "low_delay",
			"-probesize", "32",
			"-fpsprobesize", "0",
			"-analyzeduration", "0",
			"-video_size", size)
	}
	args = append(args,
		// Warning: the camera driver may decide another framerate. Sadly this fact
		// is output by ffmpeg at info level, not warning level. Use the "-v" flag
		// to see it. It looks like:
		//	[video4linux2,v4l2 @ 0x63b48c816180] The driver changed the time per frame from 1/15 to 1/10
		"-framerate", strconv.Itoa(fps),
	)
	args = append(args, "-i", src)
	if mask != "" {
		args = append(args, "-i", mask)
	} else {
		args = append(args, "-f", "lavfi", "-i", "color=color=white:size=32x32")
	}
	fg := constructFilterGraph(style, size)
	hlsOut := "[out]"
	// MJPEG stream
	if mjpeg {
		fg = append(fg,
			stream{
				sources: []string{"[out]"},
				chain:   buildChain("split=2"),
				sinks:   []string{"[outHLS]", "[out2]"},
			},
			stream{
				sources: []string{"[out2]"},
				// scaleHalf ?
				chain: buildChain("fps=fps=1"),
				sinks: []string{"[outMPJPEG]"},
			},
		)
		hlsOut = "[outHLS]"
	}
	args = append(args,
		"-filter_complex", fg.String(),
	)
	if d > 0 {
		// Limit runtime for local testing.
		// https://ffmpeg.org/ffmpeg-utils.html#time-duration-syntax
		args = append(args, "-t", fmt.Sprintf("%.1fs", float64(d)/float64(time.Second)))
	}

	// HLS in h264:
	args = append(args,
		"-map", hlsOut,
		"-c:v", "h264",
		"-preset", "fast",
		"-crf", "30",
		"-f", "hls",
		"-hls_list_size", "0",
		"-strftime", "1",
		"-hls_allow_cache", "1",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", "%Y-%m-%dT%H-%M-%S.ts",
		"all.m3u8",
	)

	// MPJPEG stream
	if mjpeg {
		// https://ffmpeg.org/ffmpeg-all.html#pipe
		args = append(args,
			"-map", "[outMPJPEG]",
			"-f", "mpjpeg",
			"-q", "2",
			//"-qscale:v", "2",
			"pipe:4",
		)
		// Sequence of images (don't forget to disable h264)
		//args = append(args, "-", "2", "output_frames_%04d.jpg")
	}
	return args, nil
}

// cmdFFMPEG constructs the *exec.Cmd to run ffmpeg.
func cmdFFMPEG(ctx context.Context, root string, args []string, handles []*os.File) *exec.Cmd {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	// stdin is intentionally not connected.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// We use pipes to transfer data (yavg metadata and mime mjeg) and not
	// stdout. This is much smarter.
	cmd.ExtraFiles = handles
	return cmd
}

func run(ctx context.Context, src, style string, d time.Duration, w, h, fps int, yavg float64, mask, root, addr, onEventStart, onEventEnd, webhook string) error {
	// References:
	// - https://ffmpeg.org/ffmpeg-all.html
	// - https://ffmpeg.org/ffmpeg-codecs.html
	// - https://ffmpeg.org/ffmpeg-formats.html
	// - https://ffmpeg.org/ffmpeg-utils.html
	// - https://trac.ffmpeg.org/wiki/Capture/Webcam
	//   ffmpeg -hide_banner -f v4l2 -list_formats all -i /dev/video3
	// - https://trac.ffmpeg.org/wiki/Encode/H.264
	size := strconv.Itoa(w) + "x" + strconv.Itoa(h)
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
	mjpeg := addr != ""
	verbose := slog.Default().Enabled(ctx, slog.LevelDebug)
	args, err := buildFFMPEGCmd(src, mask, size, fps, d, style, mjpeg, verbose)
	if err != nil {
		return err
	}
	ch := make(chan motionLevel, 10)
	events := make(chan motionEvent, 10)
	eg, ctx := errgroup.WithContext(ctx)
	slog.Debug("running", "cmd", args)
	// If any of the eg.Go() call below returns an error, this will kill ffmpeg
	// via ctx.
	// #nosec G204
	cmd := cmdFFMPEG(ctx, root, args, []*os.File{metadataW, mjpegW})
	start := time.Now().Round(10 * time.Millisecond)
	if err = cmd.Start(); err != nil {
		return nil
	}
	_ = metadataW.Close()
	metadataW = nil
	_ = mjpegW.Close()
	mjpegW = nil
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
