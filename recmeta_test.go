package main

import (
	"testing"
	"time"
)

func TestGuessLegacy(t *testing.T) {
	chs := []Channel{
		{ID: "1", URL: "https://twitch.tv/shroud", Plugin: "twitch"},
		{ID: "2", URL: "https://kick.com/xqc", Name: "xQc", Plugin: "kick"},
		{ID: "3", URL: "https://twitch.tv/a_b", Plugin: "twitch"},
	}
	mod := time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
	cases := []struct {
		file, streamer, platform string
		date                     string // "" = falls back to mtime
	}{
		{"shroud_2026-11-06_13-11-57.mp4", "shroud", "twitch", "2026-11-06 13:11:57"},
		{"twitch_shroud_2026-11-06_13-11-57.mp4", "shroud", "twitch", "2026-11-06 13:11:57"}, // platform first
		{"2026-11-06_13-11-57_shroud_twitch.mp4", "shroud", "twitch", "2026-11-06 13:11:57"}, // date first
		{"kick_xqc_2026-01-02_03-04-05.mp4", "xQc", "kick", "2026-01-02 03:04:05"},
		{"twitch_a_b_2026-01-02_03-04-05.ts", "a_b", "twitch", "2026-01-02 03:04:05"},
		{"twitch_unknown_person_2026-01-02_03-04-05.mp4", "unknown_person", "twitch", "2026-01-02 03:04:05"}, // unknown channel
		{"youtube_someone.mp4", "someone", "youtube", ""},
	}
	for _, c := range cases {
		st, pl, ts := guessLegacy(c.file, mod, chs)
		if st != c.streamer || pl != c.platform {
			t.Errorf("%s: got streamer=%q platform=%q, want %q %q", c.file, st, pl, c.streamer, c.platform)
		}
		want := mod
		if c.date != "" {
			want, _ = time.ParseInLocation("2006-01-02 15:04:05", c.date, time.Local)
		}
		if !ts.Equal(want) {
			t.Errorf("%s: got time %v, want %v", c.file, ts, want)
		}
	}
}
