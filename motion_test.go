// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestProcessMetadata(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		pr, pw := io.Pipe()
		ch := make(chan yLevel, 10)
		start := time.Now()
		errCh := make(chan error, 1)
		go func() {
			errCh <- processMetadata(start, pr, ch)
		}()

		if _, err := fmt.Fprintln(pw, "frame:1336 pts:1336    pts_time:53.44"); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(pw, "lavfi.signalstats.YAVG=0.213281"); err != nil {
			t.Fatal(err)
		}
		if err := pw.Close(); err != nil {
			t.Fatal(err)
		}

		got := <-ch
		if got.frame != 1336 {
			t.Errorf("frame: got %d, want 1336", got.frame)
		}
		// 0.213281 rounds to 0.21
		if got.yavg < 0.20 || got.yavg > 0.22 {
			t.Errorf("yavg: got %f, want ~0.21", got.yavg)
		}
		expected := start.Add(53440 * time.Millisecond).Round(100 * time.Millisecond)
		if !got.t.Equal(expected) {
			t.Errorf("t: got %v, want %v", got.t, expected)
		}
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("multiple_frames", func(t *testing.T) {
		pr, pw := io.Pipe()
		ch := make(chan yLevel, 10)
		start := time.Now()
		errCh := make(chan error, 1)
		go func() {
			err := processMetadata(start, pr, ch)
			close(ch)
			errCh <- err
		}()

		for i := range 3 {
			if _, err := fmt.Fprintf(pw, "frame:%d pts:%d    pts_time:%d.00\n", i+1, i+1, i+1); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(pw, "lavfi.signalstats.YAVG=%.6f\n", float64(i+1)*0.1); err != nil {
				t.Fatal(err)
			}
		}
		if err := pw.Close(); err != nil {
			t.Fatal(err)
		}

		count := 0
		for range ch {
			count++
		}
		if count != 3 {
			t.Errorf("got %d frames, want 3", count)
		}
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("eof", func(t *testing.T) {
		pr, pw := io.Pipe()
		ch := make(chan yLevel, 10)
		start := time.Now()
		errCh := make(chan error, 1)
		go func() {
			errCh <- processMetadata(start, pr, ch)
		}()

		if err := pw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-errCh; err != nil {
			t.Errorf("expected nil on EOF, got %v", err)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		pr, pw := io.Pipe()
		ch := make(chan yLevel, 10)
		start := time.Now()
		errCh := make(chan error, 1)
		go func() {
			errCh <- processMetadata(start, pr, ch)
		}()

		if _, err := fmt.Fprintln(pw, "not valid metadata at all"); err != nil {
			t.Fatal(err)
		}
		if err := pw.Close(); err != nil {
			t.Fatal(err)
		}

		if err := <-errCh; err == nil {
			t.Fatal("expected error for invalid metadata, got nil")
		}
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

		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

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

		go func() {
			time.Sleep(50 * time.Millisecond)
			close(ch)
		}()

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
		time.Sleep(300 * time.Millisecond)
		if evt := <-events; evt.start {
			t.Errorf("expected motion end event")
		}

		close(ch)
		if err := <-errCh; err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
