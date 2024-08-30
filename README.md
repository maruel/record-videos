# record-videos

- Streams video over MJPEG
- Detects motion
- Save recording to disk
- Integrates with [Home Assistant](#integration-with-home-assistant)

To be used along with https://github.com/maruel/serve-videos.


## Installation

1. Install prerequisites [FFmpeg](https://ffmpeg.org/download.html) and [Go](https://go.dev/dl).
2. Install `record-videos`:

```
go install github.com/maruel/record-videos@latest
```


## Usage

Help with the command line arguments available:

```
record-videos -help
```


### macOS

The command will look like:

```
record-videos -camera "FaceTime HD Camera" -fps 30 -root out
```


### linux

Install as a systemd service:

```
sudo apt install ffmpeg
mkdir -p $HOME/.config/systemd/user
git clone https://github.com/maruel/record_videos
cp record_videos/rsc/record_videos.service $HOME/.config/systemd/user
# Confirm the flags are what you want:
nano $HOME/.config/systemd/user/record_videos.service
systemctl --user daemon-reload
systemctl --user enable record_videos
systemctl --user restart record_videos
# Confirm that it works:
journalctl --user -f -u record_videos
```


### Raspberry Pi


#### Remote

For older Raspberry Pis like Zero Wireless, it's better to stream remotely the
compressed h264 stream and process the video elsewhere, e.g. on the same server
that hosts your Home Assistant server. This generates about 19KB/s. We provide a
systemd service file
[rsc/raspivid_listen.service](https://github.com/maruel/record-videos/blob/main/rsc/raspivid_listen.service)
for it.

```
ssh raspberrypi.local "mkdir -p .config/systemd/user"
scp rsc/raspivid_listen.service raspberrypi.local:.config/systemd/user
ssh raspberrypi.local "systemctl --user daemon-reload && systemctl --user enable raspivid_listen && systemctl --user restart raspivid_listen"
# Confirm it started correctly:
ssh raspberrypi.local "journalctl --user -f -u raspivid_listen"
```

Then run locally:

```
record-videos -camera tcp://raspiberrypi.local:8081 -w 1280 -h 720
```


#### Local

**Warning**: Untested.

On more recent RPis (3~4), you can run record-videos on the Pi itself. First,
cross compile from your local machine:

```
go install github.com/periph/bootstrap/cmd/push@latest
git clone https://github.com/maruel/record-videos
cd record-videos
push -host raspberrypi.local
```

Then configure it via `ssh raspberrypi.local` like you would do on a linux
machine as described above.

**Warning**: Raspberry Pi 5 has not hardware video encoder.


### Advanced

- Try `-style motion` or `-style both` to visualize the underlying data.
- Try `-mask` to only do motion detection on a subset of the frame, e.g. ignore
  the street in frame.


### Integration with Home Assistant

record-videos integrates with [Home Assistant](https://home-assistant.io/)!


#### MJPEG camera

Enable record-videos MJPEG stream output with `-addr :8081`, then connect with
https://home-assistant.io/integrations/mjpeg/ and specify
`http://127.0.0.1:8081/mjpeg` with the right IP address.


#### Motion detection

**1**: Add the following to your Home Assistant `configuration.yaml` then
restart Home Assistant.

```
# https://home-assistant.io/integrations/template/
template:
  - trigger:
      - platform: webhook
        webhook_id: my_motion_detector_INSERT_RANDOM_STRING
        local_only: true
    binary_sensor:
      - name: "My Motion Detector"
        unique_id: my_motion_detector
        state: "{{trigger.json.motion}}"
        device_class: motion
```

**2**: Start `record-videos` with the argument:
  `-webhook http://homeassistant.local:8123/api/webhook/my_motion_detector_INSERT_RANDOM_STRING`

**3**: Add the camera and motion detection signal to Home Assistant's
[UI](https://home-assistant.io/dashboards/)
and/or create an [automation](https://home-assistant.io/docs/automation/).
