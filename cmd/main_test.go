/*
Copyright 2026 Konstantinos Kalyvas.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"log/slog"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	t.Parallel()

	tests := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"INFO":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for value, want := range tests {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			got, err := parseLogLevel(value)
			if err != nil {
				t.Fatalf("parseLogLevel(%q) returned an error: %v", value, err)
			}
			if got != want {
				t.Fatalf("parseLogLevel(%q) = %v, want %v", value, got, want)
			}
		})
	}
}

func TestParseLogLevelRejectsUnknownValue(t *testing.T) {
	t.Parallel()
	if _, err := parseLogLevel("trace"); err == nil {
		t.Fatal("parseLogLevel(trace) unexpectedly succeeded")
	}
}
