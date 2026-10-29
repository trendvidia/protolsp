// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package main

import (
	"log/slog"
	"reflect"
	"testing"
)

func TestParseOrigins(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty returns nil", "", nil},
		{"single entry", "https://app.example", []string{"https://app.example"}},
		{"comma split", "https://a,https://b", []string{"https://a", "https://b"}},
		{"trims whitespace", " https://a , https://b ", []string{"https://a", "https://b"}},
		{"drops empty entries", "https://a,,https://b,", []string{"https://a", "https://b"}},
		{"whitespace-only entries are dropped", " , ,", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOrigins(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseOrigins(%q) = %#v; want %#v", tc.in, got, tc.want)
			}
			if !reflect.DeepEqual(got, tc.want) && (len(got) > 0 || len(tc.want) > 0) {
				t.Fatalf("parseOrigins(%q) = %#v; want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewLogger_LevelMapping(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want slog.Level
	}{
		{"debug", "debug", slog.LevelDebug},
		{"info", "info", slog.LevelInfo},
		{"warn", "warn", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"unknown falls back to info", "trace", slog.LevelInfo},
		{"empty falls back to info", "", slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger := newLogger(tc.in)
			if logger == nil {
				t.Fatal("newLogger returned nil")
			}
			// Probe the level by checking Enabled at the chosen level.
			if !logger.Enabled(t.Context(), tc.want) {
				t.Fatalf("logger should be enabled at %v for input %q", tc.want, tc.in)
			}
			// And NOT enabled one level below (when below Debug exists).
			if tc.want > slog.LevelDebug {
				if logger.Enabled(t.Context(), tc.want-4) {
					t.Fatalf("logger should NOT be enabled at %v for input %q", tc.want-4, tc.in)
				}
			}
		})
	}
}
