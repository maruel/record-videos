# record-videos

- High performance: uses 50% less power than Motion enabling high quality
  streams.
- Detects motion accurately via edge detection on the time blended difference.
- Efficiently save *continuous* recording to disk.
- Seamlessly integrate with [Home Assistant](#integration-with-home-assistant)
  - Streams frames over MJPEG for live viewing.
  - Define a motion sensor via webhook.


## Installation

1. Install prerequisites: [FFmpeg](https://ffmpeg.org/download.html) and [Go](https://go.dev/dl).
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
record-videos -src "FaceTime HD Camera" -fps 30 -root out
```

TODO: Describe how to set as a server via
[launchd](https://support.apple.com/guide/terminal/apdc6c1077b-5d5d-4d35-9c19-60f2397b2369/mac).


### linux

Install as a systemd service:

```
sudo apt install ffmpeg
mkdir -p $HOME/.config/systemd/user
# Enables user's services to start at boot without explicitly logging in.
loginctl enable-linger
git clone https://github.com/maruel/record-videos
cp record-videos/rsc/record-videos.service $HOME/.config/systemd/user
# Confirm the flags are what you want:
nano $HOME/.config/systemd/user/record-videos.service
systemctl --user daemon-reload
systemctl --user enable record-videos
systemctl --user restart record-videos
# Confirm that it works:
journalctl --user -f -u record-videos
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
ssh raspberrypi.local "loginctl enable-linger && systemctl --user daemon-reload && systemctl --user enable raspivid_listen && systemctl --user restart raspivid_listen"
# Confirm it started correctly:
ssh raspberrypi.local "journalctl --user -f -u raspivid_listen"
```

Then run locally:

```
record-videos -src tcp://raspiberrypi.local:8081 -w 1280 -h 720
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
- `-src` can be a local camera like `FaceTime HD Camera` on macOS, `/dev/video0`
  on linux, `tcp://192.168.1.2:8081` to connect to a Raspberry Pi running
  raspivid but it can be the current desktop! See
  https://trac.ffmpeg.org/wiki/Capture/Desktop to learn how to. **untested**


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

![Home Assistant Dashboard](https://github.com/user-attachments/assets/a1c65608-d96f-4b74-b0e5-af992026b822)

The two entities that created this:

1) Camera:
```
show_state: false
show_name: false
camera_view: live
type: picture-entity
entity: camera.my_webcam
camera_image: camera.my_webcam
aspect_ratio: 16x9
```

2) Motion detector:

```
type: entities
entities:
  - entity: binary_sensor.my_motion_detector
      secondary_info: last-changed
      show_header_toggle: false
```

