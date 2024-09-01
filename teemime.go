// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/textproto"
	"sync"
)

type mimePart struct {
	hdr textproto.MIMEHeader
	b   []byte
}

type listener struct {
	ctx context.Context
	ch  chan mimePart
}

// teeMimePart duplicates mime multipart to multiple readers.
type teeMimePart struct {
	mu        sync.Mutex
	last      mimePart
	listeners []*listener
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
		if errors.Is(err, io.EOF) {
			// We're done.
			return nil
		}
		if err != nil {
			return err
		}
		pkt := mimePart{p.Header, b}
		t.mu.Lock()
		t.last = pkt
		l := make([]*listener, len(t.listeners))
		copy(l, t.listeners)
		t.mu.Unlock()
		for _, x := range l {
			select {
			case x.ch <- pkt:
			case <-done:
				return ctx.Err()
			case <-x.ctx.Done():
				return x.ctx.Err()
			default:
				// Steal the current frame then inject another one. This permits to
				// have the channel always with a fresh frame.
				select {
				case <-x.ch:
				case <-done:
					return ctx.Err()
				case <-x.ctx.Done():
					return x.ctx.Err()
				default:
				}
				select {
				case x.ch <- pkt:
				case <-done:
					return ctx.Err()
				case <-x.ctx.Done():
					return x.ctx.Err()
				default:
				}
			}
		}
	}
	return nil
}

// relay relays data tee'd from the source.
func (b *teeMimePart) relay(ctx context.Context) <-chan mimePart {
	l := &listener{ctx, make(chan mimePart, 1)}
	b.mu.Lock()
	b.listeners = append(b.listeners, l)
	last := b.last
	b.mu.Unlock()
	// Inject the last packet right away.
	if len(last.hdr) != 0 && len(last.b) != 0 {
		l.ch <- last
	}
	go func() {
		defer func() {
			close(l.ch)
			b.mu.Lock()
			for i := range b.listeners {
				if b.listeners[i] == l {
					copy(b.listeners[i:], b.listeners[i+1:])
					b.listeners = b.listeners[:len(b.listeners)-1]
					break
				}
			}
			b.mu.Unlock()

		}()
		<-ctx.Done()
	}()
	return l.ch
}
