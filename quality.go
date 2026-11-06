package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Smart quality selection.
//
// Streamlink names video streams "<height>p" for up to 30 fps and
// "<height>p<fps>" above that (720p, 720p60, 1440p120); duplicates of the same
// name get a suffix such as "_alt". A broadcast therefore offers EITHER 720p OR
// 720p60, and a fixed "720p" setting fails on 60 fps streams (and vice versa).
//
// The user may just write "720p". Given the names the channel really offers
// (from `streamlink --json`) resolveQuality picks the right variant:
//
//	720p    -> 720p60 on a 60 fps stream, 720p on a 30 fps one (highest fps at that height)
//	720p60  -> the same, but prefers 60 fps if both exist
//	720p30  -> the same, but prefers the 30 fps variant if both exist
//	900p    -> height not offered: the nearest LOWER one, else the nearest higher
//
// A comma-separated list is a priority list ("1080p60,720p,best"): the first
// entry whose height exists wins. If none exists, the closest height to the
// first resolution entry is used, then best/worst/other special names.

var (
	streamNameRe = regexp.MustCompile(`^(\d+)p(\d+)?(_[a-z0-9_]+)?$`) // available stream: 720p, 720p60, 720p60_alt
	tokenRe      = regexp.MustCompile(`^(\d+)p(\d+)?$`)               // user request: 720p, 720p60
)

type streamInfo struct {
	Name   string
	Height int
	FPS    int
	Alt    bool // has a suffix such as _alt / _alt2
}

func parseStreams(names []string) (res []streamInfo) {
	for _, n := range names {
		m := streamNameRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(n)))
		if m == nil {
			continue
		}
		h, _ := strconv.Atoi(m[1])
		fps := 30
		if m[2] != "" {
			fps, _ = strconv.Atoi(m[2])
		}
		res = append(res, streamInfo{Name: n, Height: h, FPS: fps, Alt: m[3] != ""})
	}
	return res
}

// pickHeight chooses the best variant at one height. prefFPS 0 = highest fps.
func pickHeight(all []streamInfo, height, prefFPS int) (streamInfo, bool) {
	var c []streamInfo
	for _, s := range all {
		if s.Height == height {
			c = append(c, s)
		}
	}
	if len(c) == 0 {
		return streamInfo{}, false
	}
	dist := func(s streamInfo) int {
		if prefFPS <= 0 {
			return -s.FPS
		}
		d := s.FPS - prefFPS
		if d < 0 {
			d = -d
		}
		return d
	}
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].Alt != c[j].Alt {
			return !c[i].Alt // regular stream before "_alt" duplicates
		}
		if di, dj := dist(c[i]), dist(c[j]); di != dj {
			return di < dj
		}
		if c[i].FPS != c[j].FPS {
			return c[i].FPS > c[j].FPS
		}
		return c[i].Name < c[j].Name
	})
	return c[0], true
}

// nearestHeight returns the closest offered height below `height`, or, if there
// is none, the closest one above it.
func nearestHeight(all []streamInfo, height int) (int, bool) {
	lower, higher := -1, -1
	for _, s := range all {
		switch {
		case s.Height < height && s.Height > lower:
			lower = s.Height
		case s.Height > height && (higher < 0 || s.Height < higher):
			higher = s.Height
		}
	}
	switch {
	case lower >= 0:
		return lower, true
	case higher >= 0:
		return higher, true
	}
	return 0, false
}

func isSpecialName(s string) bool {
	switch s {
	case "best", "worst", "best-unfiltered", "worst-unfiltered":
		return true
	}
	return false
}

// QualityChoice is the outcome of resolveQuality.
type QualityChoice struct {
	Name string // concrete stream name chosen ("720p60"), "best"/"worst", or "" if unknown
	Arg  string // value for streamlink's positional STREAM argument (always has a fallback)
}

// resolveQuality turns the user's quality setting into a stream name that
// exists. available are the names reported by `streamlink --json` (may be empty
// when unknown, then the setting is passed through with a "best" fallback).
func resolveQuality(spec string, available []string) QualityChoice {
	var tokens []string
	for _, t := range strings.Split(spec, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	if len(tokens) == 0 {
		tokens = []string{"best"}
	}
	passthrough := func() QualityChoice {
		arg := strings.Join(tokens, ",")
		if !containsSpecial(tokens) {
			arg += ",best"
		}
		return QualityChoice{Arg: arg}
	}
	if len(available) == 0 {
		return passthrough()
	}
	all := parseStreams(available)
	exact := map[string]string{}
	for _, n := range available {
		exact[strings.ToLower(n)] = n
	}
	done := func(name string) QualityChoice {
		if name == "best" || name == "worst" {
			return QualityChoice{Name: name, Arg: name}
		}
		return QualityChoice{Name: name, Arg: name + ",best"}
	}

	// A: resolution entries (same height, fps-aware) and exact names, in order
	firstH, firstFPS := -1, 0
	for _, tok := range tokens {
		low := strings.ToLower(tok)
		if isSpecialName(low) {
			continue
		}
		if m := tokenRe.FindStringSubmatch(low); m != nil {
			h, _ := strconv.Atoi(m[1])
			fps := 0
			if m[2] != "" {
				fps, _ = strconv.Atoi(m[2])
			}
			if firstH < 0 {
				firstH, firstFPS = h, fps
			}
			if s, ok := pickHeight(all, h, fps); ok {
				return done(s.Name)
			}
			continue
		}
		if n, ok := exact[low]; ok { // audio_only, source, hd, ...
			return done(n)
		}
	}
	// B: requested height is not offered -> closest one
	if firstH >= 0 {
		if h, ok := nearestHeight(all, firstH); ok {
			if s, ok := pickHeight(all, h, firstFPS); ok {
				return done(s.Name)
			}
		}
	}
	// C: special synonyms, otherwise best
	for _, tok := range tokens {
		if low := strings.ToLower(tok); isSpecialName(low) {
			return done(low)
		}
	}
	return done("best")
}

func containsSpecial(tokens []string) bool {
	for _, t := range tokens {
		if isSpecialName(strings.ToLower(t)) {
			return true
		}
	}
	return false
}
