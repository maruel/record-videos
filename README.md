# Records movements from a camera as videos to disk

To be used along with https://github.com/maruel/serve-videos.


## Installation

First, install [FFmpeg](https://ffmpeg.org/download.html) and [Go](https://go.dev/dl).

Then install `record-videos`:

    go install github.com/maruel/record-videos@latest


## Usage

Help with the command line arguments available:

    record-videos -help


### macOS

The command will look like:

    record-videos -camera "FaceTime HD Camera" -fps 30


### linux

The command will look like:

    record-videos -camera /dev/video0


### Advanced

- Try `-style motion` or `-style both`.
- Try `-mask` to only do motion detection on a subset of the frame.
