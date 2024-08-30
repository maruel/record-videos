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

    record-videos -camera "FaceTime HD Camera" \
      -fps 30 -root out


### linux

The command will look like:

    record-videos -camera /dev/video0 \
      -w 960 -h 720 --root out


### Advanced

- Try `-style motion` or `-style both` to visualize the underlying data.
- Try `-mask` to only do motion detection on a subset of the frame, e.g. ignore
  the street in frame.


### Integration with Home Assistant

record-videos integrates with [Home Assistant](https://home-assistant.io/)!


#### MJPEG camera

Enable MJPEG output with `-addr :8081`, then connect with
https://home-assistant.io/integrations/mjpeg/ and specify
`http://127.0.0.1:8081/` with the right IP address.


#### Motion detection

**1**: Create two scripts and make them as executable with `chmod +x`. Replace
`homeassistant.local` with its IP address.

`on_event_start.sh`:

```
#!/bin/bash
set -eu
echo "Event detected"
curl -X POST -sS -H "Content-Type: application/json" \
  -d '{"motion":true}' \
  http://homeassistant.local:8123/api/webhook/my_motion_detector_INSERT_RANDOM_STRING
```

`on_event_end.sh`:

```
#!/bin/bash
set -eu
echo "Event detected"
curl -X POST -sS -H "Content-Type: application/json" \
  -d '{"motion":false}' \
  http://homeassistant.local:8123/api/webhook/my_motion_detector_INSERT_RANDOM_STRING
```


**2**: Add the following to your Home Assistant `configuration.yaml` then
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

**3**: Start `record-videos` with the argument:
`-on-event-start scripts/on_event_start.sh -on-event-end scripts/on_event_end.sh`


**4**: Add the camera and motion detection signal to Home Assistant's
[UI](https://home-assistant.io/dashboards/)
and/or create an [automation](https://home-assistant.io/docs/automation/).
