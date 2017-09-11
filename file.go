package main

import (
	"os"
	"path/filepath"
	"strings"
)

type File struct {
	ID   string
	Info os.FileInfo
	Path string
}

func (f File) Transcoding() bool {
	return ActiveTranscode(f.Path)
}

func (f File) Clickable() bool {
	switch f.Ext() {
	case "jpg", "jpeg", "gif", "png", "txt", "pdf":
		return true
	}
	return false
}

func (f File) Base() string {
	return filepath.Base(f.Info.Name())
}

func (f File) Ext() string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(f.Info.Name())), ".")
}

func (f File) Viewable() bool {
	if strings.Contains(f.Base(), "sample") {
		return false
	}
	switch f.Ext() {
	case "mp4", "m4v", "m4a", "m4b", "mp3":
		return true
	}
	return false
}

func (f File) Convertible() bool {
	switch f.Ext() {
	case "avi", "flv", "mov", "mkv", "webm", "wma":
		return true
	}
	return false
}

func (f File) Thumbnail() bool {
	_, err := os.Stat(f.Path + ".thumbnail.png")
	return err == nil
}
