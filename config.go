package main

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
)

type Config struct {
	sync.RWMutex
	filename string

	// Settings
	Ratio float64 `json:"ratio"`
}

func NewConfig(filename string) (*Config, error) {
	filename = filepath.Join(downloadDir, filename)
	c := &Config{filename: filename}
	b, err := ioutil.ReadFile(filename)

	// Default for new config
	if os.IsNotExist(err) {
		c.Ratio = 1.5
		return c, c.Save()
	}
	if err != nil {
		return nil, err
	}

	// Open existing config
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) Get() Config {
	c.RLock()
	defer c.RUnlock()

	return Config{
		Ratio: c.Ratio,
	}
}

func (c *Config) SetRatio(n float64) error {
	c.Lock()
	c.Ratio = n
	c.Unlock()
	return c.Save()
}

func (c *Config) Save() error {
	c.RLock()
	defer c.RUnlock()

	b, err := json.MarshalIndent(c, "", "    ")
	if err != nil {
		return err
	}
	return Overwrite(c.filename, b, 0644)
}
