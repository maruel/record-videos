// Copyright 2024 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import "testing"

func Test(t *testing.T) {
	// Just make sure it doesn't crash.
	constructFilterGraph("normal", 640, 480)
	constructFilterGraph("normal_no_mask", 640, 480)
	constructFilterGraph("motion_only", 640, 480)
	constructFilterGraph("both", 640, 480)
}
