// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import "testing"

func Test(t *testing.T) {
	// Just make sure it doesn't crash.
	for _, s := range validStyles {
		constructFilterGraph(s, 640, 480)
	}
}
