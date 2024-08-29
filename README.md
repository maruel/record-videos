# Serves a directory of videos over HTTP

Mainly to see video recordings from motion.


## Installation

First, install [FFmpeg](https://ffmpeg.org/download.html) and [Go](https://go.dev/dl).

Then install `record-videos`:

    go install github.com/maruel/record-videos@latest


## Usage


### macOS

The command will look like:

    record-videos -camera "FaceTime HD Camera" -w 1280 -h 720 -fps 30


### linux

The command will look like:

    record-videos -camera /dev/video0

Help with the command line arguments available:

    record-videos -help
