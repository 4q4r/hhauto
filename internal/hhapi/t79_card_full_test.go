package hhapi

// T79: the /vacancy/{id} SSR card nests its payload in
// vacancyView.vacancyFull.vacancy (live-probed 2026-09-30: the flat
// vacancyView carries none of the content — every card parsed empty and the
// cover-letter path died with the "vacancy record is empty" refusals). The
// publication date rides under publicationTimeIso there.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The nested vacancyFull.vacancy shape parses name, employer (the company
// block), description and the publicationTimeIso date through VacancyCard.
func TestVacancyCardVacancyFullT79(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/vacancy/777003", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(readFixture(t, "vacancy_full.html"))
	})
	c := env.client()
	v, err := c.VacancyCard(context.Background(), 777003)
	if err != nil {
		t.Fatalf("VacancyCard: %v", err)
	}
	if v.Name != "Erlang разработчик" {
		t.Fatalf("name = %q, want the nested vacancy title", v.Name)
	}
	if v.EmployerID != 9003 || v.EmployerName != "Corp Notes" {
		t.Fatalf("employer = %d/%q, want 9003/Corp Notes", v.EmployerID, v.EmployerName)
	}
	if !strings.Contains(v.Description, "sre/devops") {
		t.Fatalf("description missing the token: %q", v.Description)
	}
	// publicationTimeIso "2026-09-30T09:23:35.403+03:00" — fractional
	// seconds must survive the parse.
	if v.PublishedAt.IsZero() || v.PublishedAt.UTC().Hour() != 6 || v.PublishedAt.UTC().Minute() != 23 || v.PublishedAt.Nanosecond() != 403000000 {
		t.Fatalf("published = %s, want 2026-09-30T09:23:35.403+03:00 parsed", v.PublishedAt)
	}
}

// The legacy flat vacancyView (no vacancyFull block) keeps parsing — the
// unwrap must never fire on it.
func TestVacancyCardFlatLegacyT79(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/vacancy/777004", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>` +
			`<template id="HH-Lux-InitialState">{&#34;vacancyView&#34;:{&#34;vacancyId&#34;:777004,&#34;name&#34;:&#34;Flat Card&#34;,&#34;published_at&#34;:&#34;2026-09-20T10:00:00+0300&#34;,&#34;employer&#34;:{&#34;id&#34;:9002,&#34;name&#34;:&#34;Legacy LLC&#34;}}}</template>` +
			`</body></html>`))
	})
	c := env.client()
	v, err := c.VacancyCard(context.Background(), 777004)
	if err != nil {
		t.Fatalf("VacancyCard: %v", err)
	}
	if v.Name != "Flat Card" || v.EmployerID != 9002 || v.EmployerName != "Legacy LLC" {
		t.Fatalf("flat card parsed as %+v, want Flat Card/9002 Legacy LLC", v)
	}
	if v.PublishedAt.IsZero() {
		t.Fatalf("published = zero, want 2026-09-20T10:00:00+0300")
	}
}
