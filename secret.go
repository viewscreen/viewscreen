package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

// Secret generates a random value and stores it in a file for persistent access.
type Secret struct {
	filename string
}

// NewSecret tries to create the secret file and return the Secret.
func NewSecret(filename string) *Secret {
	s := &Secret{filename: filename}
	s.Get()
	return s
}

// Get returns the secret, creating it if necessary.
func (s Secret) Get() string {
	// Write the value if it doesn't exist already.
	if _, err := os.Stat(s.filename); os.IsNotExist(err) {
		if err := s.Reset(); err != nil {
			panic(err)
		}
	}
	// Read the value that must exist now.
	value, err := ioutil.ReadFile(s.filename)
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(value))
}

// Reset generates and writes a new secret to the file.
func (s Secret) Reset() error {
	n, err := RandomNumber()
	if err != nil {
		return err
	}
	content := []byte(fmt.Sprintf("%d\n", n))

	tmpfile, err := ioutil.TempFile(filepath.Dir(s.filename), ".tmpsecret")
	if err != nil {
		return err
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write(content); err != nil {
		return err
	}
	return os.Rename(tmpfile.Name(), s.filename)
}
