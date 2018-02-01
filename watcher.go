package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/viewscreen/viewscreen/internal/downloader"
)

var ErrDownloadNotFound = errors.New("download not found")
var ErrFileNotFound = errors.New("download file not found")
var ErrFriendNotFound = errors.New("friend not found")

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
		dl := Download{
			ID:      dir.Name(),
			Created: dir.ModTime(),
		}

		// Skip downloads that are currently transferring.
		if dl.Downloading() {
			continue
		}
		dls = append(dls, dl)
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

func IsUploading(id string) bool {
	return dler.Uploading(id)
}

func IsDownloading(id string) bool {
	return dler.Downloading(id)
}

//
// Transfers
//

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

//
// Transcoding
//

func StartTranscode(path string) error {
	return tcer.Add(path)
}

func CancelTranscode(path string) error {
	return tcer.Cancel(path)
}

func ActiveTranscode(path string) bool {
	return tcer.Active(path)
}

//
// Friends
//

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
