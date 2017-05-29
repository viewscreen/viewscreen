package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tricklecloud/trickle/internal/downloader"
)

var ErrDownloadNotFound = errors.New("download not found")
var ErrFileNotFound = errors.New("download file not found")
var ErrFriendNotFound = errors.New("friend not found")

//
// Secret
//

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

//
// Friend
//

type Friend struct {
	ID    string
	Error error
}

type FriendDownload struct {
	ID   string
	Size int64
}

type FriendFile struct {
	ID   string
	Size int64
}

func (f *Friend) Downloads() []FriendDownload {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	endpoint := fmt.Sprintf("https://%s/trickle/v1/downloads?friend=%s", f.ID, httpHost)

	res, err := GET(ctx, endpoint)
	if err != nil {
		f.Error = err
		return nil
	}

	b, err := ioutil.ReadAll(io.LimitReader(res.Body, httpReadLimit))
	if err != nil {
		f.Error = err
		return nil
	}

	var downloads []FriendDownload
	if err := json.Unmarshal(b, &downloads); err != nil {
		f.Error = err
		return nil
	}
	return downloads
}

//
// Download
//

type Download struct {
	ID       string
	Target   string
	Complete bool
}

func (dl Download) Sharefile() string {
	return filepath.Join(downloadDir, ".shared", dl.ID)
}

func (dl Download) Shared() bool {
	if _, err := os.Stat(dl.Sharefile()); err == nil {
		return true
	}
	return false
}

func (dl Download) Share() error {
	if dl.Shared() {
		return nil
	}
	// Ensure the sharing directory exists first.
	path := filepath.Dir(dl.Sharefile())
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	_, err := os.Create(dl.Sharefile())
	return err
}

func (dl Download) Unshare() error {
	if !dl.Shared() {
		return nil
	}
	return os.Remove(dl.Sharefile())
}

func (dl Download) Path() string {
	path := filepath.Join(downloadDir, dl.ID)
	path = filepath.Clean(path)
	if path == downloadDir {
		panic("invalid or missing download ID")
	}
	return path
}

func (dl Download) Size() int64 {
	var size int64
	filepath.Walk(dl.Path(), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		size += info.Size()
		return nil
	})
	return size
}

func (dl Download) Files() []File {
	var files []File
	filepath.Walk(dl.Path(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}

		// The ID is a relative path from the download's path.
		id := path
		id = strings.TrimPrefix(id, dl.Path())
		id = strings.TrimPrefix(id, "/")

		files = append(files, File{
			ID:   id,
			Info: info,
			Path: path,
		})
		return nil
	})
	return files
}

func (dl Download) FindFile(id string) (File, error) {
	for _, file := range dl.Files() {
		if id == file.ID {
			return file, nil
		}
	}
	return File{}, ErrFileNotFound
}

//
// File
//

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
	case "jpg", "jpeg", "gif", "png", "txt", "pdf", "mp3":
		return true
	}
	return false
}

func (f File) Ext() string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(f.Info.Name())), ".")
}
func (f File) Viewable() bool {
	switch f.Ext() {
	case "mp4", "m4v", "m4b", "mp3":
		return true
	}
	return false
}

func (f File) Convertible() bool {
	switch f.Ext() {
	case "avi", "flv", "mov", "mkv", "webm":
		return true
	}
	return false
}

//
// Downloads
//

func ListDownloads() ([]Download, error) {
	dirs, _, err := ls(downloadDir)
	if err != nil {
		return nil, err
	}

	var dls []Download
	for _, dir := range dirs {
		dls = append(dls, Download{
			ID: dir.Name(),
		})
	}
	return dls, nil
}

func FindDownload(id string) (Download, error) {
	dls, err := ListDownloads()
	if err != nil {
		return Download{}, err
	}
	for _, dl := range dls {
		if id == dl.ID {
			return dl, nil
		}
	}
	return Download{}, ErrDownloadNotFound
}

func ActiveDownload(id string) (bool, error) {
	dirs, _, err := ls(incomingDir)
	if err != nil {
		return false, err
	}
	for _, dir := range dirs {
		if dir.Name() == id {
			return true, nil
		}
	}
	return false, nil
}

// Transfers
func ListTransfers() []downloader.Transfer {
	return dler.ListActive()
}

func ListTransfersPending() []downloader.Transfer {
	return dler.ListPending()
}

func StartTransfer(target string) error {
	_, err := dler.Add(target)
	return err
}

func CancelTransfer(id string) error {
	return dler.Remove(id)
}

func FindTransfer(id string) (downloader.Transfer, error) {
	return dler.Find(id)
}

// Transcoding

func StartTranscode(path string) error {
	return tcer.Add(path)
}

func CancelTranscode(path string) error {
	return tcer.Cancel(path)
}

func ActiveTranscode(path string) bool {
	return tcer.Active(path)
}

func FriendHostfile(host string) string {
	return filepath.Join(friendsDir, host)
}

func AddFriend(host string) error {
	if metadata {
		_, err := POST(nil, fmt.Sprintf("http://169.254.169.254/v1/links?host=%s", host))
		if err != nil {
			return err
		}
		return nil
	}
	_, err := os.Create(FriendHostfile(host))
	return err
}

func RemoveFriend(host string) error {
	if metadata {
		_, err := DELETE(nil, fmt.Sprintf("http://169.254.169.254/v1/links?host=%s", host))
		if err != nil {
			return err
		}
		return nil
	}
	return os.Remove(FriendHostfile(host))
}

func ListFriends() ([]Friend, error) {
	if metadata {
		res, err := GET(nil, "http://169.254.169.254/v1/links")
		if err != nil {
			return nil, err
		}

		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}

		if string(b) == "" {
			return nil, nil
		}

		hosts := strings.Split(strings.TrimSpace(string(b)), "\n")

		var friends []Friend
		for _, host := range hosts {
			friends = append(friends, Friend{ID: host})
		}
		return friends, nil
	}

	_, files, err := ls(friendsDir)
	if err != nil {
		return nil, err
	}
	var friends []Friend
	for _, f := range files {
		friends = append(friends, Friend{ID: f.Name()})
	}
	return friends, nil
}

func FindFriend(host string) (Friend, error) {
	friends, err := ListFriends()
	if err != nil {
		return Friend{}, err
	}
	for _, f := range friends {
		if host == f.ID {
			return f, nil
		}
	}
	return Friend{}, ErrFriendNotFound
}
