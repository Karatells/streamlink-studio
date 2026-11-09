package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Channel is a monitored stream source. Secrets are persisted here but are
// never sent to the browser (see ChannelView in api.go).
type Channel struct {
	ID               string    `json:"id"`
	URL              string    `json:"url"`
	Name             string    `json:"name"`
	Plugin           string    `json:"plugin"`
	Quality          string    `json:"quality"`
	AutoRecord       bool      `json:"auto_record"`
	CheckInterval    int       `json:"check_interval"` // seconds, 0 = default
	FilenameTemplate string    `json:"filename_template"`
	AuthMode         string    `json:"auth_mode"` // "global" | "custom"
	OAuthToken       string    `json:"oauth_token"`
	DisableAds       bool      `json:"disable_ads"`
	LowLatency       bool      `json:"low_latency"`
	HasCookies       bool      `json:"has_cookies"`
	Proxy            string    `json:"proxy"`
	Headers          string    `json:"headers"`
	Email            string    `json:"email"`
	Password         string    `json:"password"`
	ExtraArgs        string    `json:"extra_args"`
	CreatedAt        time.Time `json:"created_at"`
}

// Settings holds global defaults and global credentials.
type Settings struct {
	TwitchToken     string   `json:"twitch_token"`
	HasCookies      bool     `json:"has_cookies"`
	Proxy           string   `json:"proxy"`
	DefaultQuality  string   `json:"default_quality"`
	DefaultInterval int      `json:"default_interval"`
	DefaultTemplate string   `json:"default_template"`
	RemuxAfter      bool     `json:"remux_after"`
	StreamlinkBin   string   `json:"streamlink_bin"`
	FFmpegBin       string   `json:"ffmpeg_bin"`
	UpdateChannel   string   `json:"update_channel"`   // "" = the channel the program was installed from
	UpdateCheckOff  bool     `json:"update_check_off"` // automatic checks disabled
	NotifyOff       []string `json:"notify_off"`       // muted notification types
	GotifyOn        bool     `json:"gotify_on"`
	GotifyURL       string   `json:"gotify_url"`
	GotifyToken     string   `json:"gotify_token"`
	GotifyTypes     []string `json:"gotify_types"` // nil = all types
	GotifyLang      string   `json:"gotify_lang"`
}

type storeData struct {
	Channels []Channel `json:"channels"`
	Settings Settings  `json:"settings"`
}

// Store is a small JSON-file database (atomic writes, mutex-protected).
type Store struct {
	mu   sync.RWMutex
	path string
	data storeData
}

func defaultSettings() Settings {
	return Settings{
		DefaultQuality:  "best",
		DefaultInterval: 60,
		DefaultTemplate: "{streamer}_{date}_{time}.mp4",
		RemuxAfter:      true,
	}
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.data.Settings = defaultSettings()
		return s, s.saveLocked()
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	if s.data.Settings.DefaultQuality == "" {
		s.data.Settings.DefaultQuality = "best"
	}
	if s.data.Settings.DefaultInterval < 15 {
		s.data.Settings.DefaultInterval = 60
	}
	if s.data.Settings.DefaultTemplate == "" {
		s.data.Settings.DefaultTemplate = "{streamer}_{date}_{time}.mp4"
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) Channels() []Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Channel(nil), s.data.Channels...)
}

func (s *Store) Channel(id string) (Channel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.data.Channels {
		if c.ID == id {
			return c, true
		}
	}
	return Channel{}, false
}

// PutChannel inserts or replaces a channel.
func (s *Store) PutChannel(c Channel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Channels {
		if s.data.Channels[i].ID == c.ID {
			s.data.Channels[i] = c
			return s.saveLocked()
		}
	}
	s.data.Channels = append(s.data.Channels, c)
	return s.saveLocked()
}

func (s *Store) DeleteChannel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.data.Channels[:0:0]
	for _, c := range s.data.Channels {
		if c.ID != id {
			out = append(out, c)
		}
	}
	s.data.Channels = out
	return s.saveLocked()
}

func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Settings
}

func (s *Store) SetSettings(st Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Settings = st
	return s.saveLocked()
}

func ensureDir(p string) error { return os.MkdirAll(p, 0o750) }

func cookiePath(dataDir, id string) string {
	return filepath.Join(dataDir, "cookies", id+".txt")
}
