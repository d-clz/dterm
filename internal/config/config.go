package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// TunnelSpec describes one SSH tunnel associated with a bookmark.
// Type is "local", "remote", or "dynamic".
//
//	local:   -L LocalPort:RemoteHost:RemotePort
//	remote:  -R RemotePort:LocalHost:LocalPort
//	dynamic: -D LocalPort  (SOCKS5 proxy)
type TunnelSpec struct {
	Name       string `yaml:"name,omitempty"`
	Type       string `yaml:"type"`
	LocalPort  int    `yaml:"local_port"`
	RemoteHost string `yaml:"remote_host,omitempty"`
	RemotePort int    `yaml:"remote_port,omitempty"`
	LocalHost  string `yaml:"local_host,omitempty"`
}

type Bookmark struct {
	Name       string       `yaml:"name"`
	Host       string       `yaml:"host"`
	Port       int          `yaml:"port"`
	User       string       `yaml:"user"`
	KeyFile    string       `yaml:"key_file"`
	Password   string       `yaml:"password,omitempty"`
	Color      string       `yaml:"color,omitempty"`
	Group      string       `yaml:"group,omitempty"`
	Tunnels    []TunnelSpec `yaml:"tunnels,omitempty"`
}

type Config struct {
	Bookmarks    []Bookmark `yaml:"bookmarks"`
	DefaultShell string     `yaml:"default_shell"`
	LogDir       string     `yaml:"log_dir"`
}

func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".dterm", "bookmarks.yaml")
}

func Load(path string) (*Config, error) {
	explicit := path != ""
	if !explicit {
		path = DefaultConfigPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && os.IsNotExist(err) {
			return defaultConfig(), nil
		}
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	for i := range cfg.Bookmarks {
		if cfg.Bookmarks[i].Port == 0 {
			cfg.Bookmarks[i].Port = 22
		}
	}
	return &cfg, nil
}

func Save(path string, cfg *Config) error {
	if path == "" {
		path = DefaultConfigPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func defaultConfig() *Config {
	return &Config{
		Bookmarks:    []Bookmark{},
		DefaultShell: "",
		LogDir:       "",
	}
}
