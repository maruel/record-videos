// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"mime/multipart"
	"net/textproto"
	"sync"
)

type mimePart struct {
	hdr textproto.MIMEHeader
	b   []byte
}

// teeMimePart duplicates mime multipart to multiple readers.
type teeMimePart struct {
	mu        sync.Mutex
	last      mimePart
	listeners []chan mimePart
}

// listen reads a mimepart stream, decodes it, then relay it to the current
// readers.
func (t *teeMimePart) listen(ctx context.Context, r io.Reader, boundary string) error {
	mr := multipart.NewReader(r, boundary)
	done := ctx.Done()
	for i := 0; ctx.Err() == nil; i++ {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			// We're done.
			return nil
		}
		if err != nil {
			return err
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return err
		}
		// First part read.
		if i == 0 {
			slog.Info("ready")
		}
		pkt := mimePart{p.Header, b}
		t.mu.Lock()
		t.last = pkt
		l := make([]chan mimePart, len(t.listeners))
		copy(l, t.listeners)
		t.mu.Unlock()
		for _, x := range l {
			select {
			case x <- pkt:
			case <-done:
				return ctx.Err()
			default:
				// Steal the current frame then inject another one. This permits to
				// have the channel always with a fresh frame.
				<-x
				select {
				case x <- pkt:
				case <-done:
					return ctx.Err()
				default:
				}
			}
		}
	}
	return nil
}

// relay relays data tee'd from the source.
func (b *teeMimePart) relay(ctx context.Context) iter.Seq2[textproto.MIMEHeader, []byte] {
	ch := make(chan mimePart, 1)
	b.mu.Lock()
	b.listeners = append(b.listeners, ch)
	last := b.last
	b.mu.Unlock()
	return func(yield func(textproto.MIMEHeader, []byte) bool) {
		defer func() {
			b.mu.Lock()
			for i := range b.listeners {
				if b.listeners[i] == ch {
					copy(b.listeners[i:], b.listeners[i+1:])
					b.listeners = b.listeners[:len(b.listeners)-1]
					break
				}
			}
			b.mu.Unlock()
		}()
		if ctx.Err() != nil || !yield(last.hdr, last.b) {
			return
		}
		// Do not leak memory.
		last.hdr = nil
		last.b = nil
		for {
			select {
			case <-ctx.Done():
				return
			case pkt := <-ch:
				if !yield(pkt.hdr, pkt.b) {
					return
				}
			}
		}
	}
}
