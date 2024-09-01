// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// "-filter_complex" argument building code. This is generic reusable code.

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

// The rest is specific to this project.

// Well known filters.
var (
	// drawTimestamp draws the current timestamp as an overlay.
	drawTimestamp filter = "drawtext@1=" +
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
	drawYAVG filter = "drawtext=" +
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
	//
	// https://ffmpeg.org/ffmpeg-filters.html#scale-1
	scaleHalf filter = "scale=w=iw/2:h=ih/2"

	// motionEdgeDetect does motion detection by calculating the edges on the delta
	// between each frame pairs.
	motionEdgeDetect = chain{
		// Do edge detection. This effectively half the frame rate.
		"tblend=all_mode=difference", "edgedetect",
		// Duplicate each frames and reset the frame time stamps.
		"tpad=stop_mode=clone:stop_duration=1", "setpts=N/FRAME_RATE/TB",
	}

	// printYAVGtoPipe prints YAVG to pipe #3 when the value is above 0.1.
	//
	// Pipe #3 is the first pipe specified in exec.Cmd.ExtraFiles.
	printYAVGtoPipe filter = "metadata=print:key=lavfi.signalstats.YAVG:file='pipe\\:3':direct=1"

	// printFilteredYAVGtoPipe prints YAVG to pipe #3 when the value is above 0.1.
	//
	// Pipe #3 is the first pipe specified in exec.Cmd.ExtraFiles.
	//
	//lint:ignore U1000 not used because of keep-alive
	printFilteredYAVGtoPipe filter = "metadata=print:key=lavfi.signalstats.YAVG:function=greater:value=0.1:file='pipe\\:3':direct=1"
)

type style string

func (s *style) Set(v string) error {
	options := ""
	for i, x := range validStyles {
		if v == string(x) {
			*s = x
			return nil
		}
		if i != 0 {
			options += ", "
		}
		options += string(x)
	}
	return errors.New("invalid style. Supported values are: " + options)
}

func (s *style) String() string {
	return string(*s)
}

// validStyles is the valid style values for constructFilterGraph.
var validStyles = []style{"normal", "normal_no_mask", "motion_only", "overlay", "both"}

// constructFilterGraph constructs the argument for -filter_complex.
func constructFilterGraph(s style, w, h int) filterGraph {
	halfSize := strconv.Itoa(w/2) + "x" + strconv.Itoa(h/2)
	switch s {
	case "normal":
		return filterGraph{
			{
				sources: []string{"[0:v]"},
				chain:   buildChain("hqdn3d", "split=2"),
				sinks:   []string{"[src1]", "[src2]"},
			},
			{
				sources: []string{"[1:v]"},
				chain:   buildChain("scale=" + halfSize),
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
				chain: buildChain("color=color=black:size=" + halfSize),
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
		return filterGraph{
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
		return filterGraph{
			{
				sources: []string{"[0:v]"},
				chain:   buildChain("hqdn3d", scaleHalf),
				sinks:   []string{"[src]"},
			},
			{
				sources: []string{"[1:v]"},
				chain:   buildChain("scale="+halfSize, "split=2"),
				sinks:   []string{"[mask1]", "[mask2]"},
			},
			{
				sources: []string{"[src][mask1]"},
				chain:   buildChain("alphamerge"),
				sinks:   []string{"[alpha]"},
			},
			{
				chain: buildChain("color=color=black:size=" + halfSize),
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
				chain: buildChain("color=color=red:size=" + halfSize),
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
				chain:   buildChain("overlay", "scale=iw*2:ih*2"),
				sinks:   []string{"[out]"},
			},
		}
	case "overlay":
		return filterGraph{
			{
				sources: []string{"[0:v]"},
				chain:   buildChain("hqdn3d", "split=2"),
				sinks:   []string{"[src1]", "[src2]"},
			},
			{
				sources: []string{"[1:v]"},
				chain:   buildChain("scale=" + halfSize),
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
				chain: buildChain("color=color=black:size=" + halfSize),
				sinks: []string{"[black]"},
			},
			{
				sources: []string{"[black][alpha]"},
				chain:   buildChain("overlay"),
				sinks:   []string{"[masked]"},
			},
			{
				sources: []string{"[masked]"},
				chain:   buildChain(motionEdgeDetect, "signalstats", printYAVGtoPipe, drawYAVG, "scale=iw*2:ih*2"),
				sinks:   []string{"[motion]"},
			},
			{
				sources: []string{"[src2]", "[motion]"},
				chain:   buildChain("blend=lighten", drawTimestamp),
				sinks:   []string{"[out]"},
			},
		}
	case "both":
		return filterGraph{
			{
				sources: []string{"[0:v]"},
				chain:   buildChain("hqdn3d", "split=2"),
				sinks:   []string{"[src1]", "[src2]"},
			},
			{
				sources: []string{"[1:v]"},
				chain:   buildChain("scale="+halfSize, "split=2"),
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
				chain: buildChain("color=color=black:size=" + halfSize),
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
				chain:   buildChain(drawTimestamp, "pad='iw*2':ih"),
				sinks:   []string{"[overlay1]"},
			},
			{
				chain: buildChain("color=color=red:size=" + halfSize),
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
				chain:   buildChain("overlay", "scale=iw*2:ih*2"),
				sinks:   []string{"[overlay2]"},
			},
			{
				sources: []string{"[overlay1][overlay2]"},
				chain:   buildChain("overlay='w'"),
				sinks:   []string{"[out]"},
			},
		}
	default:
		panic("unknown style " + s)
	}
}

// ffmpegOptions is the options to pass to ffmpeg to retrieve the video.
type ffmpegOptions struct {
	// src is the source video.
	src string
	// mask is an optional file path to a mask.
	mask string
	// w, h, fps are frame size and frame rate.
	w, h, fps int
	// d is the optional duration limit of recording, mainly for testing.
	d time.Duration
	// s controls the video format generated, see style's documentation.
	s style
	// codec is one of h264 or libx265. libx265 takes about twice the CPU usage.
	codec string
	// mjpeg determines is the MJPEG stream is enabled.
	mjpeg bool
	// verbose increases ffmpeg's output.
	verbose bool

	_ struct{}
}

// buildFFMPEGCmd builds the command line to exec ffmpeg.
//
// Outputs:
// - HLS and all.m3u8 into the current working directory.
// - YAVG metadata to the first pipe in ExtraFiles.
// - Mime encoded MJPEG to the second pipe in ExtraFiles, if mjpeg is true.
func buildFFMPEGCmd(o *ffmpegOptions) ([]string, error) {
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
	if o.verbose {
		// If you still struggle, you can use debug to get filtergraph information
		// per frame. The amount of data generated is impractical in steady state.
		//	args = append(args, "-loglevel", "repeat+debug")
		args = append(args, "-loglevel", "repeat+info")
	} else {
		args = append(args, "-loglevel", "repeat+warning")
	}
	if strings.HasPrefix(o.src, "tcp://") {
		// This is hardcoding the raspivid use case. Create an issue if this is a
		// problem.
		args = append(args, "-f", "h264")
	} else {
		switch runtime.GOOS {
		case "darwin":
			args = append(args, "-f", "avfoundation")
		case "linux":
			args = append(args, "-f", "v4l2")
		case "windows":
			args = append(args, "-f", "dshow")
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
			"-video_size", strconv.Itoa(o.w)+"x"+strconv.Itoa(o.h))
	}
	args = append(args,
		// Warning: the camera driver may decide another framerate. Sadly this fact
		// is output by ffmpeg at info level, not warning level. Use the "-v" flag
		// to see it. It looks like:
		//	[video4linux2,v4l2 @ 0x63b48c816180] The driver changed the time per frame from 1/15 to 1/10
		"-framerate", strconv.Itoa(o.fps),
	)
	args = append(args, "-i", o.src)
	if o.mask != "" {
		args = append(args, "-i", o.mask)
	} else {
		args = append(args, "-f", "lavfi", "-i", "color=color=white:size=32x32")
	}
	fg := constructFilterGraph(o.s, o.w, o.h)
	hlsOut := "[out]"
	// MJPEG stream (optional)
	if o.mjpeg {
		// Append the mjpeg specific filterGraph.
		fg = append(fg,
			stream{
				sources: []string{"[out]"},
				chain:   buildChain("split=2"),
				sinks:   []string{"[outHLS]", "[out2]"},
			},
			// TODO: Select the frame with the highest YAVG value in the past second.
			// This would increase jitter slightly but would make a much better
			// visual when in style "motion_only" or "both".
			stream{
				sources: []string{"[out2]"},
				chain:   buildChain("fps=fps=1"),
				sinks:   []string{"[outMPJPEG]"},
			},
		)
		hlsOut = "[outHLS]"
	}
	args = append(args,
		"-filter_complex", fg.String(),
	)
	if o.d > 0 {
		// Limit runtime for local testing.
		// https://ffmpeg.org/ffmpeg-utils.html#time-duration-syntax
		args = append(args, "-t", fmt.Sprintf("%.1fs", float64(o.d)/float64(time.Second)))
	}

	// HLS:
	args = append(args,
		"-map", hlsOut,
		"-c:v", o.codec,
		"-preset", "fast",
		"-crf", "30",
		"-f", "hls",
		"-metadata", "service_provider='https://github.com/maruel/record-videos'",
		"-metadata", "service_name='ffmpeg'",
		"-hls_list_size", "0",
		"-strftime", "1",
		"-hls_allow_cache", "1",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", "%Y-%m-%dT%H-%M-%S.ts",
		"all.m3u8",
	)

	// MPJPEG stream
	if o.mjpeg {
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
	slog.Debug("exec", "args", args)
	// #nosec G204
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = root
	// stdin is intentionally not connected.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// We use pipes to transfer data (yavg metadata and mime mjeg) and not
	// stdout. This is much smarter.
	cmd.ExtraFiles = handles
	return cmd
}
