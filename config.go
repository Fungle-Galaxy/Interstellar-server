package main

import (
	"fmt"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

// ICEServer mirrors the WebRTC RTCIceServer shape plus the BCL region extension.
type ICEServer struct {
	URLs       any    `yaml:"urls" json:"urls"` // string or []string
	Username   string `yaml:"username,omitempty" json:"username,omitempty"`
	Credential string `yaml:"credential,omitempty" json:"credential,omitempty"`
	// Region is a free-form tag (e.g. "cn", "hk", "jp") that is passed through
	// to clients untouched. Clients can use it to let the user pick / auto-rank
	// which TURN server to use for relay fallback and NAT-fix mode.
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
}

// Validate checks urls are a string or list of stun:/turn:/turns: URIs.
func (s ICEServer) Validate() error {
	check := func(u string) error {
		parsed, err := url.Parse(u)
		if err != nil {
			return fmt.Errorf("bad url %q: %v", u, err)
		}
		switch parsed.Scheme {
		case "stun", "stuns", "turn", "turns":
			return nil
		}
		return fmt.Errorf("url %q: scheme must be stun/stuns/turn/turns", u)
	}
	switch v := s.URLs.(type) {
	case string:
		return check(v)
	case []string:
		for _, u := range v {
			if err := check(u); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, u := range v {
			str, ok := u.(string)
			if !ok {
				return fmt.Errorf("urls list entries must be strings, got %T", u)
			}
			if err := check(str); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("urls must be a string or a list of strings, got %T", s.URLs)
}

// IsTURN reports whether this entry is a TURN (relay) server.
func (s ICEServer) IsTURN() bool {
	first := ""
	switch v := s.URLs.(type) {
	case string:
		first = v
	case []string:
		if len(v) > 0 {
			first = v[0]
		}
	case []any:
		if len(v) > 0 {
			if str, ok := v[0].(string); ok {
				first = str
			}
		}
	}
	return len(first) >= 4 && (first[:4] == "turn" )
}

type IntegratedRelaySettings struct {
	Enabled         bool     `yaml:"enabled"`
	ListeningIps    []string `yaml:"listeningIps"`
	RelayIps        []string `yaml:"relayIps"`
	ExternalIps     []string `yaml:"externalIps"`
	MinPort         int      `yaml:"minPort"`
	MaxPort         int      `yaml:"maxPort"`
	ListeningPort   int      `yaml:"listeningPort"`
	DebugLevel      string   `yaml:"debugLevel"`
	DefaultUsername string   `yaml:"defaultUsername"`
	DefaultPassword string   `yaml:"defaultPassword"`
}

type PeerConfig struct {
	ForceRelayOnly  bool                   `yaml:"forceRelayOnly"`
	IntegratedRelay IntegratedRelaySettings `yaml:"integratedRelay"`
	ICEServers      []ICEServer            `yaml:"iceServers"`
}

func DefaultPeerConfig() PeerConfig {
	return PeerConfig{
		ForceRelayOnly: false,
		IntegratedRelay: IntegratedRelaySettings{
			Enabled:         false,
			ListeningIps:    []string{"0.0.0.0"},
			RelayIps:        []string{},
			ExternalIps:     []string{},
			MinPort:         49152,
			MaxPort:         65535,
			ListeningPort:   3478,
			DebugLevel:      "INFO",
			DefaultUsername: "M9DRVaByiujoXeuYAAAG",
			DefaultPassword: "TpHR9HQNZ8taxjb3",
		},
		ICEServers: []ICEServer{
			{URLs: "stun:stun.l.google.com:19302"},
		},
	}
}

// LoadPeerConfig reads a YAML file over the defaults. Fields absent from the
// file keep their default values (nicer than the TS server, which replaced the
// whole config and crashed when integratedRelay was missing).
func LoadPeerConfig(path string) (PeerConfig, error) {
	cfg := DefaultPeerConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			logf("INFO", "config file %s not found, using defaults", path)
			return cfg, nil
		}
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("unable to load peer config file. Make sure it is valid YAML: %v", err)
	}
	// Normalize YAML list values into []string for urls.
	for i := range cfg.ICEServers {
		if list, ok := cfg.ICEServers[i].URLs.([]any); ok {
			out := make([]string, 0, len(list))
			for _, v := range list {
				str, ok := v.(string)
				if !ok {
					return cfg, fmt.Errorf("iceServers[%d].urls entries must be strings", i)
				}
				out = append(out, str)
			}
			cfg.ICEServers[i].URLs = out
		}
		if err := cfg.ICEServers[i].Validate(); err != nil {
			return cfg, fmt.Errorf("iceServers[%d]: %v", i, err)
		}
	}
	if cfg.IntegratedRelay.MinPort <= 0 || cfg.IntegratedRelay.MaxPort > 65535 || cfg.IntegratedRelay.MinPort > cfg.IntegratedRelay.MaxPort {
		return cfg, fmt.Errorf("integratedRelay: invalid minPort/maxPort range")
	}
	return cfg, nil
}
