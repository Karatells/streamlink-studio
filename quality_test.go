package main

import "testing"

func TestResolveQuality(t *testing.T) {
	twitch60 := []string{"audio_only", "160p", "360p", "480p", "720p60", "1080p60"}
	twitch30 := []string{"audio_only", "160p", "360p", "480p", "720p"}
	both := []string{"480p", "720p", "720p60", "720p60_alt"}
	youtube := []string{"144p", "240p", "360p", "480p", "720p", "1080p60", "720p60"}
	hi := []string{"360p", "720p60", "1080p60", "1440p120"}

	cases := []struct {
		name  string
		spec  string
		avail []string
		want  string
		arg   string
	}{
		// the main scenario: plain "720p" adapts to the stream's frame rate
		{"720p on 60fps", "720p", twitch60, "720p60", "720p60,best"},
		{"720p on 30fps", "720p", twitch30, "720p", "720p,best"},
		{"720p60 on 30fps", "720p60", twitch30, "720p", "720p,best"},
		{"720p30 on 60fps", "720p30", twitch60, "720p60", "720p60,best"},
		{"1080p on 60fps", "1080p", twitch60, "1080p60", "1080p60,best"},
		{"exact 1080p60", "1080p60", twitch60, "1080p60", "1080p60,best"},
		// both variants offered -> highest fps unless told otherwise; _alt never preferred
		{"both, auto", "720p", both, "720p60", "720p60,best"},
		{"both, want 30", "720p30", both, "720p", "720p,best"},
		{"both, want 60", "720p60", both, "720p60", "720p60,best"},
		// height not offered -> nearest lower, else nearest higher
		{"1080p on 30fps stream", "1080p", twitch30, "720p", "720p,best"},
		{"900p", "900p", twitch60, "720p60", "720p60,best"},
		{"144p when lowest is 160p", "144p", twitch60, "160p", "160p,best"},
		{"fallback list", "1080p60,720p60,best", twitch30, "720p", "720p,best"},
		{"list prefers earlier entry", "1080p,720p", twitch60, "1080p60", "1080p60,best"},
		{"list, first missing", "1440p,720p", twitch60, "720p60", "720p60,best"},
		// youtube mixes 30 and 60 fps heights
		{"youtube 720p", "720p", youtube, "720p60", "720p60,best"},
		{"youtube 1080p", "1080p", youtube, "1080p60", "1080p60,best"},
		// high fps
		{"1440p", "1440p", hi, "1440p120", "1440p120,best"},
		{"2160p -> nearest lower", "2160p", hi, "1440p120", "1440p120,best"},
		// special / opaque names
		{"best", "best", twitch60, "best", "best"},
		{"worst", "worst", twitch60, "worst", "worst"},
		{"audio_only", "audio_only", twitch60, "audio_only", "audio_only,best"},
		{"source then 720p", "source,720p", twitch60, "720p60", "720p60,best"},
		{"case-insensitive", "720P", twitch60, "720p60", "720p60,best"},
		{"unknown name -> best", "hd", twitch60, "best", "best"},
		{"empty spec", "", twitch60, "best", "best"},
		// unknown availability: pass through but always keep a fallback
		{"no info", "720p", nil, "", "720p,best"},
		{"no info list", "720p,480p", nil, "", "720p,480p,best"},
		{"no info best", "best", nil, "", "best"},
	}
	for _, c := range cases {
		got := resolveQuality(c.spec, c.avail)
		if got.Name != c.want || got.Arg != c.arg {
			t.Errorf("%s: resolveQuality(%q) = {%q %q}, want {%q %q}", c.name, c.spec, got.Name, got.Arg, c.want, c.arg)
		}
	}
}

func TestParseStreams(t *testing.T) {
	got := parseStreams([]string{"720p60", "720p", "audio_only", "720p60_alt2", "1440p120", "worst"})
	if len(got) != 4 {
		t.Fatalf("expected 4 resolution streams, got %d: %+v", len(got), got)
	}
	if got[1].FPS != 30 || got[0].FPS != 60 || !got[2].Alt || got[3].FPS != 120 {
		t.Errorf("unexpected parse: %+v", got)
	}
}
