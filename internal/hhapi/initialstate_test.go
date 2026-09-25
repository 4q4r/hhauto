package hhapi

// T24: the SSR state carrier's attribute order drifted live (2026-09-25:
// <template style="display:none" id="HH-Lux-InitialState"> — style BEFORE
// id), which the literal-prefix extraction missed and surfaced as
// errNilState on every document surface.

import (
	"strings"
	"testing"
)

func TestExtractInitialStateToleratesAttributeOrder(t *testing.T) {
	tests := []struct {
		name string
		page string
		want bool
	}{
		{
			name: "id first (T18 capture shape)",
			page: `<html><template id="HH-Lux-InitialState">{"a":1}</template></html>`,
			want: true,
		},
		{
			name: "attributes before id (live 2026-09-25 shape)",
			page: `<html><template style="display:none" id="HH-Lux-InitialState">{"a":1}</template></html>`,
			want: true,
		},
		{
			name: "entity-encoded payload with attributes before id",
			page: `<template data-x="1" style="display:none" id="HH-Lux-InitialState">{&#34;a&#34;:&#34;b&#34;}</template>`,
			want: true,
		},
		{
			name: "window fallback still works",
			page: `<script>window.__INITIAL_STATE__ = {"a":2};</script>`,
			want: true,
		},
		{name: "no state at all", page: `<html><body>plain</body></html>`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractInitialStateJSON(tt.page)
			if ok != tt.want {
				t.Fatalf("extractInitialStateJSON ok = %v, want %v", ok, tt.want)
			}
			if !ok {
				return
			}
			// Trailing markup must never leak into the payload (the live
			// page carries </template><!--/noindex--></body> right after
			// the state node).
			if len(got) == 0 || got[0] != '{' || got[len(got)-1] != '}' {
				t.Fatalf("payload is not a balanced JSON object: %.80q", got)
			}
			if s := string(got); strings.Contains(s, "<") {
				t.Fatalf("payload contains markup: %q", s)
			}
		})
	}
}
