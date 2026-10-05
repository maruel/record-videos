// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProcessMetadata(t *testing.T) {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	t.Run("valid", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			input string
			want  []yLevel
		}{
			{name: "eof"},
			{
				name:  "frame",
				input: "frame:1336 pts:1336    pts_time:53.44\nlavfi.signalstats.YAVG=0.213281\n",
				want:  []yLevel{{frame: 1336, t: start.Add(53400 * time.Millisecond), yavg: 0.21}},
			},
			{
				name:  "multiple_frames",
				input: "frame:1 pts:1 pts_time:1.00\nlavfi.signalstats.YAVG=0.100000\nframe:2 pts:2 pts_time:2.00\nlavfi.signalstats.YAVG=0.200000\n",
				want:  []yLevel{{frame: 1, t: start.Add(time.Second), yavg: 0.1}, {frame: 2, t: start.Add(2 * time.Second), yavg: 0.2}},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ch := make(chan yLevel, 10)
				if err := processMetadata(start, strings.NewReader(tc.input), ch); err != nil {
					t.Fatal(err)
				}
				close(ch)
				var got []yLevel
				for l := range ch {
					got = append(got, l)
				}
				if !slices.Equal(got, tc.want) {
					t.Errorf("got %v, want %v", got, tc.want)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		for _, tc := range []struct{ name, input string }{
			{"invalid_line", "not valid metadata at all\n"},
			{"invalid_frame", "frame:bad pts:1 pts_time:1.00\n"},
			{"invalid_timestamp", "frame:1 pts:1 pts_time:bad\n"},
			{"invalid_yavg", "frame:1 pts:1 pts_time:1.00\nlavfi.signalstats.YAVG=bad\n"},
			{"oversized_line", strings.Repeat("x", 64*1024)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ch := make(chan yLevel, 10)
				if err := processMetadata(start, strings.NewReader(tc.input), ch); err == nil {
					t.Fatal("expected malformed metadata error")
				}
				if len(ch) != 0 {
					t.Error("malformed metadata emitted a frame")
				}
			})
		}
		t.Run("read", func(t *testing.T) {
			pr, pw := io.Pipe()
			want := errors.New("metadata read failed")
			if err := pw.CloseWithError(want); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pr.Close() })
			if err := processMetadata(start, pr, make(chan yLevel, 1)); !errors.Is(err, want) {
				t.Errorf("got %v, want %v", err, want)
			}
		})
	})
}

func TestFilterMotion(t *testing.T) {
	t.Run("keep_alive", func(t *testing.T) {
		ch := make(chan yLevel)
		events := make(chan motionEvent, 10)
		mo := &motionOptions{
			yThreshold:       1.0,
			motionExpiration: time.Second,
			keepAlive:        100 * time.Millisecond,
		}

		err := filterMotion(t.Context(), mo, time.Now(), ch, events)
		if err == nil {
			t.Fatal("expected keep-alive error, got nil")
		}
		if !strings.Contains(err.Error(), "no events") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("context_cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		ch := make(chan yLevel)
		events := make(chan motionEvent, 10)
		mo := &motionOptions{
			yThreshold:       1.0,
			motionExpiration: time.Second,
			keepAlive:        5 * time.Second,
		}

		cancel()

		if err := filterMotion(ctx, mo, time.Now(), ch, events); err != nil {
			t.Errorf("expected nil on context cancel, got %v", err)
		}
	})

	t.Run("channel_close", func(t *testing.T) {
		ch := make(chan yLevel)
		events := make(chan motionEvent, 10)
		mo := &motionOptions{
			yThreshold:       1.0,
			motionExpiration: time.Second,
			keepAlive:        5 * time.Second,
		}

		close(ch)

		if err := filterMotion(t.Context(), mo, time.Now(), ch, events); err != nil {
			t.Errorf("expected nil on channel close, got %v", err)
		}
	})

	t.Run("detection", func(t *testing.T) {
		ch := make(chan yLevel, 10)
		events := make(chan motionEvent, 10)
		mo := &motionOptions{
			yThreshold:       0.1,
			motionExpiration: 200 * time.Millisecond,
			keepAlive:        5 * time.Second,
		}
		start := time.Now()

		errCh := make(chan error, 1)
		go func() {
			errCh <- filterMotion(t.Context(), mo, start, ch, events)
		}()

		// Send a frame above the threshold to trigger motion start.
		ch <- yLevel{frame: 1, t: start, yavg: 0.5}
		if evt := <-events; !evt.start {
			t.Errorf("expected motion start event")
		}

		// Wait for motion expiration.
		if evt := <-events; evt.start {
			t.Errorf("expected motion end event")
		}

		close(ch)
		if err := <-errCh; err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
