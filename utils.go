package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type DiskInfo struct {
	free int64
	used int64
}

func (d *DiskInfo) Total() int64   { return d.free + d.used }
func (d *DiskInfo) TotalMB() int64 { return d.Total() / 1024 / 1024 }
func (d *DiskInfo) TotalGB() int64 { return d.TotalMB() / 1024 }

func (d *DiskInfo) Free() int64   { return d.free }
func (d *DiskInfo) FreeMB() int64 { return d.free / 1024 / 1024 }
func (d *DiskInfo) FreeGB() int64 { return d.FreeMB() / 1024 }

func (d *DiskInfo) Used() int64   { return d.used }
func (d *DiskInfo) UsedMB() int64 { return d.used / 1024 / 1024 }
func (d *DiskInfo) UsedGB() int64 { return d.UsedMB() / 1024 }

func (d *DiskInfo) UsedPercent() float64 {
	return (float64(d.used) / float64(d.Total())) * 100
}

func NewDiskInfo(path string) (*DiskInfo, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, fmt.Errorf("diskinfo failed: %s", err)
	}
	free := stat.Bavail * uint64(stat.Bsize)
	used := (stat.Blocks * uint64(stat.Bsize)) - free
	return &DiskInfo{int64(free), int64(used)}, nil
}

func ls(path string) ([]os.FileInfo, []os.FileInfo, error) {
	list, err := ioutil.ReadDir(path)
	if err != nil {
		return nil, nil, err
	}
	var dirs []os.FileInfo
	var files []os.FileInfo
	for _, f := range list {
		if strings.HasSuffix(f.Name(), "thumbnail.png") { // skip thumbnail files
			continue
		}
		if strings.HasPrefix(f.Name(), ".") { // skip hidden files
			continue
		}
		if f.IsDir() {
			dirs = append(dirs, f)
		} else {
			files = append(files, f)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[j].Name() > dirs[i].Name() })
	sort.Slice(files, func(i, j int) bool { return files[j].Name() > files[i].Name() })
	return dirs, files, nil
}

func GET(ctx context.Context, rawurl string) (*http.Response, error) {
	return request("GET", ctx, rawurl)
}

func POST(ctx context.Context, rawurl string) (*http.Response, error) {
	return request("POST", ctx, rawurl)
}

func DELETE(ctx context.Context, rawurl string) (*http.Response, error) {
	return request("DELETE", ctx, rawurl)
}

const httpUserAgent = "Mozilla/5.0 (Windows NT 5.1; rv:13.0) Gecko/20100101 Firefox/13.0.1"

func request(method string, ctx context.Context, rawurl string) (*http.Response, error) {
	// TODO: investigate issues with sharing an HTTP client across requests, which would be more efficient.
	httpClient := &http.Client{}

	req, err := http.NewRequest(method, rawurl, nil)
	if err != nil {
		return nil, err
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	} else {
		httpClient.Timeout = 10 * time.Second
	}
	req.Header.Set("User-Agent", httpUserAgent)

	logger.Debugf("HTTP request: %s %s", req.Method, req.URL)
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 400 {
		return nil, fmt.Errorf("request failed: %s", http.StatusText(res.StatusCode))
	}
	return res, nil
}

func RandomNumber() (int, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return 0, err
	}
	return int(binary.LittleEndian.Uint32(b)), nil
}

func Overwrite(filename string, data []byte, perm os.FileMode) error {
	f, err := ioutil.TempFile(filepath.Dir(filename), filepath.Base(filename)+".tmp")
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), perm); err != nil {
		return err
	}
	return os.Rename(f.Name(), filename)
}
