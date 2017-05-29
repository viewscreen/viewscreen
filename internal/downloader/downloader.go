package downloader

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"

	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	humanize "github.com/dustin/go-humanize"
	"golang.org/x/time/rate"
)

var (
	ErrTransferNotFound    = errors.New("download not found")
	ErrInsufficientStorage = errors.New("insufficient storage")

	transferSlots = 3

	httpReadLimit int64 = 10 * (1024 * 1024) // 2 MB

	torrentMaxUploadMbits   = 25 * (1024 * 1024)
	torrentMaxDownloadMbits = 200 * (1024 * 1024)
)

type Downloader struct {
	mu        sync.RWMutex
	dldir     string
	indir     string
	torrent   *torrent.Client
	storage   func() int64
	transfers []*Transfer
}

func NewDownloader(dldir, indir, taddr string, storage func() int64) (*Downloader, error) {
	// Remove any existing incoming dir.
	if len(indir) <= 1 {
		return nil, fmt.Errorf("invalid incoming dir")
	}
	os.RemoveAll(indir)
	if err := os.MkdirAll(indir, 0755); err != nil {
		return nil, err
	}

	// Create the download dir, if necessary.
	if _, err := os.Stat(dldir); os.IsNotExist(err) {
		if err := os.MkdirAll(dldir, 0755); err != nil {
			return nil, err
		}
	}

	client, err := torrent.NewClient(&torrent.Config{
		DataDir:             indir,
		UploadRateLimiter:   rate.NewLimiter(rate.Limit(torrentMaxUploadMbits/8), torrentMaxUploadMbits/4),
		DownloadRateLimiter: rate.NewLimiter(rate.Limit(torrentMaxDownloadMbits/8), torrentMaxDownloadMbits/4),
		ListenAddr:          taddr,
		Seed:                false,
		Debug:               false,
	})
	if err != nil {
		return nil, err
	}

	l := &Downloader{
		torrent: client,
		storage: storage,
		dldir:   dldir,
		indir:   indir,
	}
	go l.manager()
	return l, nil
}

type Transfer struct {
	ID        string
	URL       *url.URL
	Created   time.Time
	Started   time.Time
	Completed time.Time
	Cancel    *context.CancelFunc

	Torrent *torrent.Torrent
	Error   error

	// Friend downloads
	DownloadID    string
	DownloadSize  int64
	DownloadInDir string
}

//
// Download
//

func (l *Downloader) RLock(loc string) {
	//log.Debugf("RLock %s", loc)
	l.mu.RLock()
}

func (l *Downloader) RUnlock(loc string) {
	//log.Debugf("RUnlock %s", loc)
	l.mu.RUnlock()
}

func (l *Downloader) Lock(loc string) {
	//log.Debugf("Lock %s", loc)
	l.mu.Lock()
}
func (l *Downloader) Unlock(loc string) {
	//log.Debugf("Unlock %s", loc)
	l.mu.Unlock()
}

func (l *Downloader) manager() {
	for {
		l.Lock("manager")
		// count active transfers
		active := 0
		for _, t := range l.transfers {
			if !t.IsActive() {
				continue
			}
			active++
		}

		for _, t := range l.transfers {
			// leave active transfers alone
			if t.IsActive() {
				continue
			}
			// clean up if completed
			if t.IsCompleted() {
				l.remove(t.ID)
				continue
			}
			// start
			if active < transferSlots {
				active++
				t.Started = time.Now()
				log.Debugf("downloader starting transfer %s %s", t.ID, t.URL)
				go l.transfer(t)
				continue
			}
		}
		l.Unlock("manager")
		time.Sleep(1 * time.Second)
	}
}

func (l *Downloader) availableStorage(size int64) bool {
	space := l.storage()
	space -= int64(float64(space) * 0.05) // reserve 5%

	if size >= space {
		log.Debugf("insufficient storage: download size %s greater than available space %s", humanize.Bytes(uint64(size)), humanize.Bytes(uint64(space)))
		return false
	}
	return true
}

func (l *Downloader) transfer(t *Transfer) {

	// setup
	ctx, cancel := context.WithCancel(context.Background())
	l.Lock("tranfer")
	t.Cancel = &cancel
	friend := t.URL.Query().Get("friend")
	path := t.URL.Path
	l.Unlock("transfer")

	var err error

	// 1. Friend Download - v1 API friend download URL (e.g. https://example.com/trickle/v1/downloads/files/<download>?friend=<host>)
	if strings.HasPrefix(path, "/trickle/v1/downloads/files/") && friend != "" {
		err = l.transferFriend(ctx, t)

		// 2. Torrent
	} else {
		err = l.transferTorrent(ctx, t)
	}

	if err != nil {
		log.Errorf("download error: %s", err)
	}

	// Clean up after transfer.
	l.Lock("cleanup")
	t.Error = err
	t.Completed = time.Now()
	l.Unlock("cleanup")
}

func (l *Downloader) transferFriend(ctx context.Context, t *Transfer) error {
	l.RLock("friend url")
	host := t.URL.Host
	path := t.URL.Path
	rawurl := t.URL.String()
	me := t.URL.Query().Get("friend")
	l.RUnlock("friend url")

	// Download friend's file list.
	res, err := GET(nil, rawurl)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	b, err := ioutil.ReadAll(io.LimitReader(res.Body, httpReadLimit))
	if err != nil {
		return err
	}

	var files []struct {
		ID   string
		Size int64
	}
	if err := json.Unmarshal(b, &files); err != nil {
		return err
	}

	if len(files) == 0 {
		return fmt.Errorf("no files found for download")
	}

	downloadID := filepath.Base(path)
	if len(downloadID) < 3 || len(downloadID) > 200 {
		return fmt.Errorf("missing or invalid download id %q found in path", downloadID)
	}

	// Total size of all files.
	var downloadSize int64
	for _, f := range files {
		downloadSize += f.Size
	}

	// Ensure we have enough storage.
	if !l.availableStorage(downloadSize) {
		return ErrInsufficientStorage
	}

	indir := filepath.Join(l.indir, downloadID)
	dldir := filepath.Join(l.dldir, downloadID)

	// Store ID for later.
	l.Lock("transfer friend id")
	t.DownloadID = downloadID
	t.DownloadSize = downloadSize
	t.DownloadInDir = indir
	l.Unlock("transfer friend id")

	// Remove the incoming directory if it exists after return.
	defer os.RemoveAll(indir)

	// Download each file.
	for _, file := range files {

		dir := filepath.Join(indir, filepath.Dir(file.ID))
		filename := filepath.Join(indir, file.ID)

		// Create directory path if necessary.
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}

		// Write file to directory.
		endpoint := fmt.Sprintf("https://%s/trickle/v1/downloads/stream/%s/%s?friend=%s", host, downloadID, file.ID, me)

		log.Debugf("Downloading friend's file %s %s", file.ID, endpoint)

		res, err := GET(ctx, endpoint)
		if err != nil {
			return fmt.Errorf("friend stream request %q failed: %s", endpoint, err)
		}
		defer res.Body.Close()

		f, err := os.Create(filename)
		if err != nil {
			return fmt.Errorf("create %q failed: %s", filename, err)
		}
		if _, err = io.Copy(f, res.Body); err != nil {
			return fmt.Errorf("copy failed for %q: %s", filename, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
	}

	// Successful download.
	return os.Rename(indir, dldir)
}

func (l *Downloader) transferTorrent(ctx context.Context, t *Transfer) error {
	l.RLock("torrent url")
	scheme := t.URL.Scheme
	rawurl := t.URL.String()
	l.RUnlock("torrent url")

	if scheme == "magnet" {
		l.Lock("torrent add magnet")
		tor, err := l.torrent.AddMagnet(rawurl)
		t.Torrent = tor
		l.Unlock("torrent add magnet")
		if err != nil {
			return err
		}
	} else if scheme == "http" || scheme == "https" {
		res, err := GET(ctx, rawurl)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		metaInfo, err := metainfo.Load(io.LimitReader(res.Body, httpReadLimit))
		if err != nil {
			return err
		}

		l.Lock("torrent http add")
		tor, err := l.torrent.AddTorrent(metaInfo)
		t.Torrent = tor
		l.Unlock("torrent http add")
		return err
	} else {
		return fmt.Errorf("invalid or unrecognized torrent")
	}

	<-t.Torrent.GotInfo()

	// ensure it's not too big
	var size int64
	for _, file := range t.Torrent.Files() {
		size += file.Length()
	}
	if !l.availableStorage(size) {
		return ErrInsufficientStorage
	}

	// Do it. Do it.
	t.Torrent.DownloadAll()
	for {
		info := t.Torrent.Info()
		// Drop and rename completed.
		if t.Torrent.BytesCompleted() >= info.TotalLength() {
			t.Torrent.Drop()

			inname := filepath.Join(l.indir, info.Name) // "/data/.dls/some dir" or "/data/.dls/some file.txt"
			dlname := filepath.Join(l.dldir, info.Name) // "/data/some dir" or "/data/some file.txt"
			fi, err := os.Stat(inname)
			if err != nil {
				return err
			}

			// If its a single file, put it in a directory e.g. "/data/file.txt" -> "/data/file/file.txt"
			if !fi.IsDir() {
				basename := strings.TrimSuffix(info.Name, filepath.Ext(info.Name))
				newdir := filepath.Join(l.dldir, basename)
				if err := os.Mkdir(newdir, 0755); err != nil {
					return err
				}
				dlname = filepath.Join(newdir, info.Name)
			}

			// Rename directory into the downloads dir.
			if err := os.Rename(inname, dlname); err != nil {
				return err
			}
			return nil
		}
		time.Sleep(1 * time.Second)
	}
}

func (l *Downloader) Busy() bool {
	l.RLock("Busy")
	defer l.RUnlock("Busy")
	return len(l.transfers) > 0
}

func (l *Downloader) ListActive() []Transfer {
	l.RLock("List")
	defer l.RUnlock("List")

	var transfers []Transfer
	for _, t := range l.transfers {
		if !t.IsActive() {
			continue
		}
		transfers = append(transfers, *t)

	}
	return transfers
}

func (l *Downloader) ListPending() []Transfer {
	l.RLock("ListPending")
	defer l.RUnlock("ListPending")

	var transfers []Transfer
	for _, t := range l.transfers {
		if t.IsActive() {
			continue
		}
		transfers = append(transfers, *t)

	}
	return transfers
}

func (l *Downloader) Active() int {
	l.RLock("Active")
	defer l.RUnlock("Active")

	n := 0
	for _, t := range l.transfers {
		if !t.IsActive() {
			continue
		}
		n++
	}
	return n
}

func (l *Downloader) Waiting() int {
	l.RLock("Waiting")
	defer l.RUnlock("Waiting")

	n := 0
	for _, t := range l.transfers {
		if t.IsCompleted() {
			continue
		}
		if t.IsActive() {
			continue
		}
		n++
	}
	return n
}

func (l *Downloader) FindByURL(rawurl string) (Transfer, error) {
	l.RLock("FindByURL")
	defer l.RUnlock("FindByURL")
	t, err := l.findByURL(rawurl)
	if err != nil {
		return Transfer{}, err
	}
	return *t, nil
}

func (l *Downloader) findByURL(rawurl string) (*Transfer, error) {
	for _, t := range l.transfers {
		if rawurl != t.URL.String() {
			continue
		}
		return t, nil
	}
	return nil, ErrTransferNotFound
}

func (l *Downloader) Find(id string) (Transfer, error) {
	l.RLock("Find")
	defer l.RUnlock("Find")
	t, err := l.findByID(id)
	if err != nil {
		return Transfer{}, err
	}
	return *t, nil
}

func (l *Downloader) findByID(id string) (*Transfer, error) {
	for _, t := range l.transfers {
		if id == t.ID {
			return t, nil
		}
	}
	return nil, ErrTransferNotFound
}

func (l *Downloader) Add(rawurl string) (Transfer, error) {
	l.Lock("Add")
	defer l.Unlock("Add")

	u, err := url.Parse(rawurl)
	if err != nil {
		return Transfer{}, err
	}
	rawurl = u.String()

	// already exists
	if t, err := l.findByURL(rawurl); err == nil {
		return *t, nil
	}

	t := &Transfer{
		ID:      fmt.Sprintf("%x", md5.Sum([]byte(u.String()))),
		URL:     u,
		Created: time.Now(),
	}
	l.transfers = append(l.transfers, t)
	return *t, nil
}

func (l *Downloader) Remove(id string) error {
	l.Lock("Remove")
	defer l.Unlock("Remove")

	t, err := l.findByID(id)
	if err != nil {
		return err
	}
	// http requests get cancel()'d
	if t.Cancel != nil {
		cancel := *t.Cancel
		cancel()
	}
	// torrents get dropped and their temp files deleted.
	if t.Torrent != nil {
		t.Torrent.Drop()
		if info := t.Torrent.Info(); info != nil {
			dir := filepath.Join(l.indir, info.Name)
			if _, err := os.Stat(dir); err == nil {
				if err := os.RemoveAll(dir); err != nil {
					return fmt.Errorf("failed to remove torrent dir %s: %s", dir, err)
				}
			}
		}
	}

	// Take it out of the transfer list.
	l.remove(id)

	return nil
}

func (l *Downloader) remove(id string) {
	var transfers []*Transfer
	for _, t := range l.transfers {
		if t.ID == id {
			continue
		}
		transfers = append(transfers, t)
	}
	l.transfers = transfers
}

//
// Transfer
//

// String returns the title of the transfer.
func (t Transfer) String() string {
	if t.DownloadID != "" {
		return t.DownloadID
	}
	if t.Torrent != nil {
		if info := t.Torrent.Info(); info != nil {
			return info.Name
		}
	}
	if dn := t.URL.Query().Get("dn"); dn != "" {
		return dn
	}
	return fmt.Sprintf("Loading %s link...", t.URL.Scheme)
}

// CompletedSize returns the downloaded bytes.
func (t Transfer) CompletedSize() int64 {
	if t.DownloadID != "" {
		n, err := du(t.DownloadInDir)
		if err != nil {
			log.Error(err)
			return 0
		}
		return n
	}
	if t.Torrent != nil {
		return t.Torrent.BytesCompleted()
	}
	return 0
}

// TotalSize returns the completed size of the download in bytes.
func (t Transfer) TotalSize() int64 {
	if t.DownloadSize > 0 {
		return t.DownloadSize
	}
	if t.Torrent != nil {
		if info := t.Torrent.Info(); info != nil {
			return info.TotalLength()
		}
	}
	return 0
}

// IsActive returns true when the transfer is started but not completed.
func (t Transfer) IsActive() bool {
	return t.IsStarted() && !t.IsCompleted()
}

// IsStarted returns true when the transfer has been started.
func (t Transfer) IsStarted() bool {
	return !t.Started.IsZero()
}

// IsCompleted returns true when the transfer has been completed.
func (t Transfer) IsCompleted() bool {
	return !t.Completed.IsZero()
}

func GET(ctx context.Context, rawurl string) (*http.Response, error) {
	httpClient := &http.Client{}

	log.Debugf("GET request: %s (%s)", rawurl, ctx)

	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	} else {
		httpClient.Timeout = 10 * time.Second
	}

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 400 {
		return nil, fmt.Errorf("request failed: %s", http.StatusText(res.StatusCode))
	}
	return res, nil
}

func du(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}
