package downloader

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	log "github.com/Sirupsen/logrus"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	humanize "github.com/dustin/go-humanize"
	"golang.org/x/time/rate"
)

var (
	ErrTransferNotFound    = errors.New("download not found")
	ErrInsufficientStorage = errors.New("insufficient storage")

	// The read limit on responses expected to be relatively small.
	httpReadLimit int64 = 10 * (1024 * 1024)

	// Default download speeds
	defaultUploadSpeed   int64   = 100
	defaultDownloadSpeed int64   = 200
	defaultTransferSlots int     = 5
	defaultTorrentRatio  float64 = 1.5
)

type Downloader struct {
	mu        sync.RWMutex
	torrent   *torrent.Client
	transfers []*Transfer

	Config *Config
}

type Config struct {
	UploadSpeed   int64
	DownloadSpeed int64
	Logger        *zap.SugaredLogger
	Space         func() int64

	TorrentAddr string

	// mu protects the below, which can be accessed safely using getters/setters.
	mu            sync.RWMutex
	TransferSlots int
	DownloadDir   string
	TorrentRatio  float64
}

func (c *Config) RLock(loc string) {
	//l.Config.Logger.Debugf("RLock %s", loc)
	c.mu.RLock()
}

func (c *Config) RUnlock(loc string) {
	//l.Config.Logger.Debugf("RUnlock %s", loc)
	c.mu.RUnlock()
}

func (c *Config) Lock(loc string) {
	//l.Config.Logger.Debugf("Lock %s", loc)
	c.mu.Lock()
}
func (c *Config) Unlock(loc string) {
	//l.Config.Logger.Debugf("Unlock %s", loc)
	c.mu.Unlock()
}

// TransferSlots
func (c *Config) GetTransferSlots() int {
	c.RLock("GetTransferSlots")
	defer c.RUnlock("GetTransferSlots")
	return c.TransferSlots
}

func (c *Config) SetTransferSlots(n int) {
	c.Lock("SetTransferSlots")
	defer c.Unlock("SetTransferSlots")
	c.TransferSlots = n
}

// DownloadDir
func (c *Config) GetDownloadDir() string {
	c.RLock("GetDownloadDir")
	defer c.RUnlock("GetDownloadDir")
	return c.DownloadDir
}

func (c *Config) SetDownloadDir(path string) {
	c.Lock("SetDownloadDir")
	defer c.Unlock("SetDownloadDir")
	c.DownloadDir = path
}

// TorrentRatio
func (c *Config) GetTorrentRatio() float64 {
	c.RLock("GetTorrentRatio")
	defer c.RUnlock("GetTorrentRatio")
	return c.TorrentRatio
}

func (c *Config) SetTorrentRatio(ratio float64) {
	c.Lock("SetTorrentRatio")
	defer c.Unlock("SetTorrentRatio")
	c.TorrentRatio = ratio
}

func NewDownloader(cfg *Config) (*Downloader, error) {
	// Defaults
	if cfg.UploadSpeed == 0 {
		cfg.UploadSpeed = defaultUploadSpeed
	}
	if cfg.DownloadSpeed == 0 {
		cfg.DownloadSpeed = defaultDownloadSpeed
	}
	if cfg.TransferSlots == 0 {
		cfg.TransferSlots = defaultTransferSlots
	}
	if cfg.TorrentRatio == 0 {
		cfg.TorrentRatio = defaultTorrentRatio
	}

	if cfg.Logger == nil {
		return nil, fmt.Errorf("a Logger is required")
	}

	// Create the download dir, if necessary.
	if _, err := os.Stat(cfg.DownloadDir); os.IsNotExist(err) {
		if err := os.MkdirAll(cfg.DownloadDir, 0750); err != nil {
			return nil, err
		}
	}

	// clean up temp files
	tmpfiles, err := func() ([]string, error) {
		files, err := ioutil.ReadDir(cfg.DownloadDir)
		if err != nil {
			return nil, err
		}
		var tmpfiles []string
		for _, file := range files {
			ext := strings.TrimPrefix(filepath.Ext(file.Name()), ".")
			if ext != "uploading" && ext != "downloading" {
				continue
			}
			tmpfiles = append(tmpfiles, filepath.Join(cfg.DownloadDir, file.Name()))
		}
		return tmpfiles, nil
	}()
	if err != nil {
		return nil, err
	}

	for _, tmp := range tmpfiles {
		cfg.Logger.Debugf("removing temp file %q", tmp)
		if err := os.Remove(tmp); err != nil {
			return nil, err
		}
	}

	// rate in bytes per second (from megabits per second)
	uprate := int((cfg.UploadSpeed * (1024 * 1024)) / 8)
	downrate := int((cfg.DownloadSpeed * (1024 * 1024)) / 8)

	client, err := torrent.NewClient(&torrent.Config{
		DataDir:             cfg.DownloadDir,
		ListenAddr:          cfg.TorrentAddr,
		UploadRateLimiter:   rate.NewLimiter(rate.Limit(uprate), uprate),
		DownloadRateLimiter: rate.NewLimiter(rate.Limit(downrate), downrate),
		Seed:                true,
		DefaultStorage: storage.NewFileWithCustomPathMaker(
			cfg.DownloadDir,
			func(baseDir string, info *metainfo.Info, infoHash metainfo.Hash) string {
				dir := baseDir
				// Individual files get a directory.
				if !info.IsDir() {
					dir = filepath.Join(baseDir, strings.TrimSuffix(info.Name, filepath.Ext(info.Name)))
				}
				// Mark this transfer
				t := Transfer{DownloadDir: dir}
				if err := t.MarkDownloading(); err != nil {
					cfg.Logger.Error(err)
				}
				return dir
			},
		),
	})
	if err != nil {
		return nil, err
	}

	l := &Downloader{
		torrent: client,
		Config:  *&cfg,
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

	DownloadDir string
	Uploading   bool
	SeedRatio   float64

	Torrent *torrent.Torrent
	Error   error

	// Friend downloads
	DownloadID   string
	DownloadSize int64
}

//
// Download
//

func (l *Downloader) RLock(loc string) {
	//l.Config.Logger.Debugf("RLock %s", loc)
	l.mu.RLock()
}

func (l *Downloader) RUnlock(loc string) {
	//l.Config.Logger.Debugf("RUnlock %s", loc)
	l.mu.RUnlock()
}

func (l *Downloader) Lock(loc string) {
	//l.Config.Logger.Debugf("Lock %s", loc)
	l.mu.Lock()
}

func (l *Downloader) Unlock(loc string) {
	//l.Config.Logger.Debugf("Unlock %s", loc)
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
			// Don't count uploading transfers
			if t.Uploading {
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
			if active < l.Config.GetTransferSlots() {
				active++
				t.Started = time.Now()
				l.Config.Logger.Debugf("downloader starting transfer %s %s", t.ID, t.URL)
				go l.transfer(t)
				continue
			}
		}
		l.Unlock("manager")
		time.Sleep(1 * time.Second)
	}
}

func (l *Downloader) availableStorage(size int64) bool {
	space := l.Config.Space()
	space -= int64(float64(space) * 0.05) // reserve 5%

	if size >= space {
		l.Config.Logger.Debugf("insufficient storage: download size %s greater than available space %s", humanize.Bytes(uint64(size)), humanize.Bytes(uint64(space)))
		return false
	}
	return true
}

func (l *Downloader) transfer(t *Transfer) {
	// Prepare
	ctx, cancel := context.WithCancel(context.Background())
	l.Lock("tranfer")
	t.Cancel = &cancel
	friend := t.URL.Query().Get("friend")
	path := t.URL.Path
	l.Unlock("transfer")

	var err error
	// Transfer
	if strings.HasPrefix(path, "/watcher/v1/downloads/files/") && friend != "" {
		// 1. Friend Download - v1 API friend download URL
		// e.g. https://example.com/watcher/v1/downloads/files/<download>?friend=<host>
		err = l.transferFriend(ctx, t)
	} else {
		// 2. Torrent
		err = l.transferTorrent(ctx, t)
	}
	if err != nil {
		l.Config.Logger.Errorf("download error: %s", err.Error())
	}

	// Clean up
	l.Lock("cleanup")
	t.Error = err
	t.Completed = time.Now()
	l.Unlock("cleanup")
}

func ffthumb(videofile, thumbfile string) error {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return err
	}

	// Create temp dir.
	tmpdir, err := ioutil.TempDir(filepath.Dir(thumbfile), ".tmpthumb")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	// Create temp thumbnails.
	output, err := exec.Command(ffmpeg,
		"-y",
		"-i", videofile,
		"-vf", "thumbnail,scale=480:270,fps=1/6",
		"-vframes", "5",
		filepath.Join(tmpdir, "thumbnail%d.png"),
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffthumb failed: %s (%s)", string(output), err)
	}

	// Copy biggest thumb to destination.
	thumbs, err := ioutil.ReadDir(tmpdir)
	if err != nil {
		return err
	}

	// Find the biggest thumbnail.
	var best string
	var biggest int64
	for _, t := range thumbs {
		if t.Size() < biggest {
			continue
		}
		biggest = t.Size()
		best = filepath.Join(tmpdir, t.Name())
	}
	if best != "" {
		return os.Rename(best, thumbfile)
	}
	return fmt.Errorf("ffthumb failed: generating failed")
}

func (l *Downloader) PostProcess(ctx context.Context, t *Transfer) error {
	files, err := t.Files()
	if err != nil {
		return err
	}

	// The biggest file has the "best" thumbnail for the whole download.
	var best string
	var biggest int64
	for _, fi := range files {
		ext := strings.TrimPrefix(filepath.Ext(fi.Name()), ".")
		switch ext {
		case "mp4", "m4v", "avi", "flv", "mov", "mkv", "webm":
			videofile := filepath.Join(t.DownloadDir, fi.Name())
			thumbfile := filepath.Join(t.DownloadDir, fi.Name()+".thumbnail.png")

			if err := ffthumb(videofile, thumbfile); err != nil {
				log.Warn(err)
				continue
			}

			if fi.Size() > biggest {
				best = thumbfile
			}
		}
	}

	if best != "" {
		if exec.Command("/bin/cp",
			"-f",
			best,
			filepath.Join(t.DownloadDir, "thumbnail.png"),
		).CombinedOutput(); err != nil {
			return err
		}
	}
	return nil
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

	dldir := filepath.Join(l.Config.GetDownloadDir(), downloadID)

	// Store ID for later.
	l.Lock("transfer friend id")
	t.DownloadID = downloadID
	t.DownloadSize = downloadSize
	t.DownloadDir = dldir
	l.Unlock("transfer friend id")

	// Mark the transfer as downloading.
	if err := t.MarkDownloading(); err != nil {
		return err
	}

	// Download each file in the list.
	for _, file := range files {

		dir := filepath.Join(dldir, filepath.Dir(file.ID))
		filename := filepath.Join(dldir, file.ID)

		// Create directory path if necessary.
		if err := os.MkdirAll(dir, 0750); err != nil {
			return err
		}

		// Write file to directory.
		endpoint := fmt.Sprintf("https://%s/watcher/v1/downloads/stream/%s/%s?friend=%s", host, downloadID, file.ID, me)

		l.Config.Logger.Debugf("Downloading friend's file %s %s", file.ID, endpoint)

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
	if err := l.PostProcess(ctx, t); err != nil {
		return err
	}
	return t.UnmarkDownloading()
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

	// Wait for info.
	<-t.Torrent.GotInfo()

	// Check if we have sufficient storage for the download.
	var size int64
	for _, file := range t.Torrent.Files() {
		size += file.Length()
	}
	if !l.availableStorage(size) {
		return ErrInsufficientStorage
	}

	info := t.Torrent.Info()

	dldir := filepath.Join(l.Config.GetDownloadDir(), t.Torrent.Info().Name)

	// Individual files get a directory.
	if !info.IsDir() {
		dldir = filepath.Join(l.Config.GetDownloadDir(), strings.TrimSuffix(info.Name, filepath.Ext(info.Name)))
	}

	l.Lock("setting DownloadDir")
	t.DownloadDir = dldir
	l.Unlock("setting DownloadDir")

	// Mark the transfer as downloading.
	if err := t.MarkDownloading(); err != nil {
		return err
	}
	// Start downloading all files in the torrent.
	t.Torrent.DownloadAll()

	ticker := time.NewTicker(3 * time.Second)
	for {
		select {
		case <-ctx.Done():
			l.Config.Logger.Infof("transfer %q canceled", t.Torrent.Info().Name)
			if err := t.UnmarkDownloading(); err != nil {
				return err
			}
			return t.UnmarkUploading()
		case <-ticker.C:
			l.RLock("get Uploading")
			uploading := t.Uploading
			target := t.SeedRatio
			l.RUnlock("get Uploading")

			if uploading {
				// Unless it's unlimited, cancel when the target ratio is reached.
				if target > 0 {
					bw := t.Torrent.Stats().DataBytesWritten
					br := t.Torrent.Info().TotalLength()

					var ratio float64
					if bw > 0 && br > 0 {
						ratio = float64(bw) / float64(br)
					}

					l.Config.Logger.Debugf("transfer is uploading written: %d read: %d ratio: %v >= target %v", bw, br, ratio, target)

					if ratio >= target {
						t.Torrent.Drop()
						return t.UnmarkUploading()
					}
				}
			} else {
				remaining := t.Torrent.BytesMissing()
				l.Config.Logger.Debugf("transfer is downloading %s remaining", humanize.Bytes(uint64(remaining)))

				if remaining == 0 {
					if err := l.PostProcess(ctx, t); err != nil {
						return err
					}
					if err := t.UnmarkDownloading(); err != nil {
						return err
					}
					if target == 0 {
						t.Torrent.Drop()
						return nil
					}

					l.Lock("setting Uploading")
					t.Uploading = true
					l.Unlock("setting Uploading")
					if err := t.MarkUploading(); err != nil {
						return err
					}
				}
			}
		}
		time.Sleep(3 * time.Second)
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

func (l *Downloader) Downloading(name string) bool {
	l.RLock("Downloading")
	defer l.RUnlock("Downloading")
	_, err := os.Stat(filepath.Join(l.Config.DownloadDir, name+".downloading"))
	return err == nil
}

func (l *Downloader) Uploading(name string) bool {
	l.RLock("Uploading")
	defer l.RUnlock("Uploading")
	_, err := os.Stat(filepath.Join(l.Config.DownloadDir, name+".uploading"))
	return err == nil
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
		ID:        fmt.Sprintf("%x", md5.Sum([]byte(u.String()))),
		URL:       u,
		Created:   time.Now(),
		SeedRatio: l.Config.GetTorrentRatio(),
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

	// Cancel
	if t.Cancel != nil {
		cancel := *t.Cancel
		cancel()
	}

	// Drop torrent.
	if t.Torrent != nil {
		t.Torrent.Drop()

		// If it's uploading, do NOT delete it (it's complete).
		if !t.Uploading {
			// Clean up the download dir, if it exists.
			if _, err := os.Stat(t.DownloadDir); err == nil {
				if err := os.RemoveAll(t.DownloadDir); err != nil {
					return err
				}
			}
		}
	}

	// Take it out of the transfer list
	l.remove(id)

	// Unmark
	if err := t.UnmarkDownloading(); err != nil {
		return err
	}
	return t.UnmarkUploading()
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
		var name string
		start := time.Now()
		if info := t.Torrent.Info(); info != nil {
			name = info.Name
		}
		seconds := time.Since(start).Seconds()
		if seconds > 0.2 {
			log.Debugf("String() took %.2f seconds", seconds)
		}
		return name
	}
	if dn := t.URL.Query().Get("dn"); dn != "" {
		return dn
	}
	return fmt.Sprintf("Loading %s link...", t.URL.Scheme)
}

// TotalSeedSize returns the length of the seeding target size in bytes.
func (t Transfer) TotalSeedSize() int64 {
	if t.Torrent != nil {
		if t.SeedRatio <= 0 {
			return 0
		}
		return int64(float64(t.TotalSize()) * t.SeedRatio)
	}
	return 0
}

// UploadedBytes returns uploaded bytes.
func (t Transfer) UploadedBytes() int64 {
	if t.Torrent != nil {
		return t.Torrent.Stats().DataBytesWritten
	}
	return 0
}

// Files returns all the files inside the download dir.
func (t Transfer) Files() ([]os.FileInfo, error) {
	return find(t.DownloadDir)
}

// DownloadedBytes returns the downloaded bytes.
func (t Transfer) DownloadedBytes() int64 {
	if t.Torrent != nil {
		start := time.Now()
		size := t.Torrent.BytesCompleted()
		seconds := time.Since(start).Seconds()
		if seconds > 0.2 {
			log.Debugf("DownloadedBytes took %.2f seconds", seconds)
		}
		return size
	}
	if t.DownloadDir != "" {
		n, _ := du(t.DownloadDir)
		return n
	}
	return 0
}

// TotalSize returns the completed size of the download in bytes.
func (t Transfer) TotalSize() int64 {
	if t.DownloadSize > 0 {
		return t.DownloadSize
	}
	if t.Torrent != nil {
		start := time.Now()
		var size int64
		if info := t.Torrent.Info(); info != nil {
			size = info.TotalLength()
		}

		seconds := time.Since(start).Seconds()
		if seconds > 0.2 {
			log.Debugf("TotalSize took %.2f seconds", seconds)
		}
		return size
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

// uploadingFile() returns the path to the uploading file
func (t Transfer) uploadingFile() string {
	return t.DownloadDir + ".uploading"
}

// MarkUploading creates the .uploading file
func (t Transfer) MarkUploading() error {
	if t.DownloadDir == "" {
		return nil
	}
	return ioutil.WriteFile(t.uploadingFile(), []byte("uploading\n"), 0640)
}

// UnmarkUploading removes the .uploading file for the upload
func (t Transfer) UnmarkUploading() error {
	if t.DownloadDir == "" {
		return nil
	}
	if _, err := os.Stat(t.uploadingFile()); os.IsNotExist(err) {
		return nil
	}
	return os.Remove(t.uploadingFile())
}

// downloadingFile() returns the path to the downloading file
func (t Transfer) downloadingFile() string {
	return t.DownloadDir + ".downloading"
}

// MarkDownloading creates the .downloading file
func (t Transfer) MarkDownloading() error {
	if t.DownloadDir == "" {
		return nil
	}
	return ioutil.WriteFile(t.downloadingFile(), []byte("downloading\n"), 0640)
}

// UnmarkDownloading removes the .downloading file for the download
func (t Transfer) UnmarkDownloading() error {
	if t.DownloadDir == "" {
		return nil
	}
	if _, err := os.Stat(t.downloadingFile()); os.IsNotExist(err) {
		return nil
	}
	return os.Remove(t.downloadingFile())
}

func GET(ctx context.Context, rawurl string) (*http.Response, error) {
	httpClient := &http.Client{}

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
	err := filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !fi.IsDir() {
			size += fi.Size()
		}
		return nil
	})
	return size, err
}

func find(path string) ([]os.FileInfo, error) {
	var files []os.FileInfo
	err := filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		files = append(files, fi)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, err
}
