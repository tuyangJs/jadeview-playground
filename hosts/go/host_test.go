package main

import (
	"math"
	"net/url"
	"testing"
)

func TestPageAllowed(t *testing.T) {
	entry, _ := url.Parse("jade://jadeview-go-playground.v")
	for _, s := range []string{"jade://jadeview-go-playground.v/index.html", "https://jade.jadeview-go-playground.v/pane.html?theme=dark"} {
		if !pageAllowed(entry, s) {
			t.Errorf("denied %s", s)
		}
	}
	for _, s := range []string{"https://jadeview-go-playground.v/index.html", "https://jade.jadeview-go-playground.v.evil.test/index.html", "https://jade.jadeview-go-playground.v/admin.html", "jade://jadeview-go-playground.v@evil.test/index.html"} {
		if pageAllowed(entry, s) {
			t.Errorf("accepted %s", s)
		}
	}
}

func TestDevURL(t *testing.T) {
	for _, s := range []string{"http://127.0.0.1:5173/", "http://[::1]:5174"} {
		if _, err := devURL(s); err != nil {
			t.Error(err)
		}
	}
	for _, s := range []string{"http://0.0.0.0:5173", "http://127.0.0.1:5173/some/app", "https://evil.test:5173", "http://127.0.0.1:5173@evil.test:5173", "http://127.0.0.1:5173/?x=y"} {
		if _, err := devURL(s); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}

func TestArgs(t *testing.T) {
	a, e := parseArgs([]string{"--dev", "--example", "layout"})
	if e != nil || a.dev != "http://127.0.0.1:5173" || a.example != "layout" {
		t.Fatalf("%+v, %v", a, e)
	}
	a, e = parseArgs([]string{"--dev", "http://127.0.0.1:5180"})
	if e != nil || a.dev != "http://127.0.0.1:5180" {
		t.Fatalf("%+v, %v", a, e)
	}
	if _, e = parseArgs([]string{"--dev", "--frontend", "web/dist"}); e == nil {
		t.Fatal("allowed conflicting flags")
	}
	if _, e = parseArgs([]string{"--smoke"}); e == nil {
		t.Fatal("smoke without an isolated data dir")
	}
}

func TestLayout(t *testing.T) {
	v, e := cells("columns", 2, rect{X: 10, Y: 20, Width: 800, Height: 500, ViewportWidth: 1000, ViewportHeight: 600}, .4)
	if e != nil || len(v) != 2 || v[0][2] != "40.000000%-83.200px" || v[1][0] != "40.000000%-65.200px" {
		t.Fatalf("%v %v", v, e)
	}
	if _, e = cells("rows", 2, rect{Width: 800, Height: 500}, math.NaN()); e == nil {
		t.Fatal("accepted NaN")
	}
	if _, e = cells("grid", 7, rect{Width: 800, Height: 500}, .5); e == nil {
		t.Fatal("accepted too many panes")
	}
}

func TestNativeLinuxOriginIsolation(t *testing.T) {
	const first = "https://127.0.0.1:42163/index.html"
	if !sameViewLoopback(first, first+"?theme=dark", "/index.html") {
		t.Fatal("rejected own native origin")
	}
	for _, candidate := range []string{
		"http://127.0.0.1:42163/index.html",
		"https://127.0.0.1:42164/index.html",
		"https://127.0.0.1:42163/pane.html",
		"https://127.0.0.1:42163@localhost:42163/index.html",
	} {
		if sameViewLoopback(first, candidate, "/index.html") {
			t.Errorf("accepted %s", candidate)
		}
	}
}
