package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/armon/circbuf"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/watchercloud/watcher/internal/downloader"
	"github.com/watchercloud/watcher/internal/search"
	"github.com/watchercloud/watcher/internal/transcoder"

	"github.com/eduncan911/podcast"
	"github.com/julienschmidt/httprouter"
	"golang.org/x/crypto/acme/autocert"
)

var (
	// Flags
	cli         = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	downloadDir string

	httpAddr     string
	httpHost     string
	httpUsername string
	httpPrefix   string

	letsencrypt bool
	metadata    bool
	backlink    string

	// usually ".friends" in the download dir.
	friendsDir string

	// The version is set by the build command.
	version string

	// torrent
	torrentListenAddr string

	// reverse proxy authentication
	reverseProxyAuthIP     string
	reverseProxyAuthHeader string

	//show version
	showVersion bool

	// debug logging
	debug bool

	// feed secret
	feedsecret *Secret
	authsecret *Secret

	// set based on httpAddr
	httpIP   string
	httpPort string

	// transcoder
	tcer *transcoder.Transcoder

	// downloader
	dler *downloader.Downloader

	// logger
	logger  *zap.SugaredLogger
	logtail *logtailer

	httpReadLimit int64 = 2 * (1024 * 1024) // 2 MB

	// config
	config *Config
)

func NewLogtailer(size int64) (*logtailer, error) {
	buf, err := circbuf.NewBuffer(size)
	if err != nil {
		return nil, err
	}
	return &logtailer{tail: buf}, nil
}

type logtailer struct {
	sync.RWMutex

	tail *circbuf.Buffer
}

func (l *logtailer) Lines() []string {
	l.RLock()
	buf := l.tail.Bytes()
	l.RUnlock()

	s := string(buf)
	start := 0
	if nl := strings.Index(s, "\n"); nl != -1 {
		start = nl + len("\n")
	}
	return strings.Split(s[start:], "\n")
}

func (l *logtailer) Write(buf []byte) (int, error) {
	l.Lock()
	n, err := l.tail.Write(buf)
	l.Unlock()
	return n, err
}

func (l *logtailer) Sync() error {
	return nil
}

func init() {
	cli.StringVar(&downloadDir, "download-dir", "/data", "download directory")
	cli.StringVar(&backlink, "backlink", "", "backlink (optional)")
	cli.StringVar(&httpAddr, "http-addr", ":80", "listen address")
	cli.StringVar(&httpHost, "http-host", "", "HTTP host")
	cli.StringVar(&httpPrefix, "http-prefix", "/watcher", "HTTP URL prefix (not supported yet)")
	cli.StringVar(&httpUsername, "http-username", "watcher", "HTTP basic auth username")
	cli.StringVar(&torrentListenAddr, "torrent-addr", ":61337", "listen address for torrent client")
	cli.StringVar(&reverseProxyAuthIP, "reverse-proxy-ip", "", "reverse proxy auth IP")
	cli.StringVar(&reverseProxyAuthHeader, "reverse-proxy-header", "X-Authenticated-User", "reverse proxy auth header")
	cli.BoolVar(&showVersion, "version", false, "display version and exit")
	cli.BoolVar(&metadata, "metadata", false, "use metadata service")
	cli.BoolVar(&letsencrypt, "letsencrypt", false, "enable TLS using Let's Encrypt")
	cli.BoolVar(&debug, "debug", false, "debug mode")
}

// Index redirect
func index(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	Redirect(w, r, "/")
}

func logs(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, line := range logtail.Lines() {
		fmt.Fprintf(w, "%s\n", line)
	}
}

//
// Downloads
//
func library(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	res := NewResponse(r, ps)
	query := strings.ToLower(strings.TrimSpace(r.FormValue("q")))

	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}

	// Top 4 recent
	recent := make([]Download, len(dls))
	copy(recent, dls)
	sort.Slice(recent, func(i, j int) bool { return recent[i].Created.After(recent[j].Created) })
	res.Downloads = make([]Download, 4)
	copy(res.Downloads, recent)

	// Filter library
	if query != "" {
		for _, dl := range dls {
			text := strings.ToLower(dl.ID)
			if !strings.Contains(text, strings.ToLower(query)) {
				continue
			}
			res.Library = append(res.Library, dl)
		}
	} else {
		res.Library = dls
	}

	res.Query = query
	res.Section = "library"
	HTML(w, "index.html", res)
}

func dlList(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}
	res := NewResponse(r, ps)
	res.Downloads = dls
	HTML(w, "downloads/list.html", res)
}

func dlFiles(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	res := NewResponse(r, ps)
	res.Download = dl
	res.Section = "files"
	HTML(w, "downloads/files.html", res)
}

func dlView(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	res := NewResponse(r, ps)
	res.Download = dl
	res.File = file
	res.Section = "view"
	HTML(w, "downloads/view.html", res)
}

func dlSave(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(file.Path)))
	http.ServeFile(w, r, file.Path)
}

func dlStream(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", 7*86400))
	http.ServeFile(w, r, file.Path)
}

func dlRemove(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := os.RemoveAll(dl.Path()); err != nil {
		Error(w, err)
		return
	}
	Redirect(w, r, "/?message=downloadremoved")
}

func dlShare(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id := ps.ByName("id")
	dl, err := FindDownload(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := dl.Share(); err != nil {
		Error(w, err)
		return
	}
	JSON(w, `{ status: "success" }`)
}

func dlUnshare(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	id := ps.ByName("id")
	dl, err := FindDownload(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := dl.Unshare(); err != nil {
		Error(w, err)
		return
	}
	JSON(w, `{ status: "success" }`)
}

//
// Transfers
//

func transferList(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	res := NewResponse(r, ps)
	res.Transfers = ListTransfers()
	res.TransfersPending = ListTransfersPending()
	HTML(w, "transfers/list.html", res)
}

func transferMagnet(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if err := StartTransfer(r.FormValue("target")); err != nil {
		Error(w, err)
		return
	}
	JSON(w, `{ status: "success" }`)
}

func transferStart(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	target := strings.TrimSpace(r.FormValue("target"))
	if target == "" {
		target = ps.ByName("target")
	}
	if err := StartTransfer(target); err != nil {
		Error(w, err)
		return
	}
	Redirect(w, r, "/import")
}

func transferCancel(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if err := CancelTransfer(ps.ByName("id")); err != nil {
		Error(w, err)
		return
	}
	Redirect(w, r, "/import")
}

//
// Transcoding
//

func transcodeStart(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	logger.Debugf("starting trancode %q", file.Path)
	if err := StartTranscode(file.Path); err != nil {
		Error(w, err)
		return
	}
	Redirect(w, r, "/downloads/files/%s", dl.ID)
}

func transcodeCancel(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	logger.Debugf("canceling trancode %q", file.Path)

	if err := CancelTranscode(file.Path); err != nil {
		Error(w, err)
		return
	}
	Redirect(w, r, "/downloads/files/%s", dl.ID)
}

//
// Friends
//

func friends(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	friends, err := ListFriends()
	if err != nil {
		Error(w, err)
		return
	}

	res := NewResponse(r, ps)
	res.Friends = friends
	res.Section = "friends"
	HTML(w, "friends.html", res)
}

var validFriendHost = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9\.\-]+$`)

func friendAdd(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	host := strings.TrimSpace(r.FormValue("host"))
	if host == "" {
		Error(w, fmt.Errorf("no friend host"))
		return
	}

	if !validFriendHost.MatchString(host) {
		Redirect(w, r, "/friends?message=friendinvalidhost")
		return
	}

	if err := AddFriend(host); err != nil {
		Error(w, err)
		return
	}
	Redirect(w, r, "/friends?message=friendadded")
}

func friendRemove(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	host := ps.ByName("host")
	if host == "" {
		Error(w, fmt.Errorf("no friend host"))
		return
	}
	if err := RemoveFriend(host); err != nil {
		Error(w, err)
		return
	}
	Redirect(w, r, "/friends?message=friendremoved")
}

func friendDownload(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	host := ps.ByName("host")
	dl := ps.ByName("dl")

	f, err := FindFriend(host)
	if err != nil {
		Error(w, err)
		return
	}

	endpoint := &url.URL{
		Scheme:   "https",
		Host:     f.ID,
		Path:     "/watcher/v1/downloads/files/" + dl,
		RawQuery: "friend=" + httpHost,
	}

	if err := StartTransfer(endpoint.String()); err != nil {
		Error(w, err)
		return
	}

	Redirect(w, r, "/?message=transferstarted")
}

//
// Settings
//

func settings(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if r.Method == "GET" {
		res := NewResponse(r, ps)
		res.Section = "settings"
		HTML(w, "settings.html", res)
		return
	}

	ratio := strings.TrimSpace(r.FormValue("ratio"))

	n, err := strconv.ParseFloat(ratio, 64)
	if err == nil {
		if err := config.SetRatio(n); err != nil {
			Error(w, err)
			return
		}
		dler.Config.SetTorrentRatio(n)
	}

	Redirect(w, r, "/settings?message=settingssaved")
}

//
// Search
//

func importHandler(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	res := NewResponse(r, ps)

	query := strings.TrimSpace(r.FormValue("q"))
	if query != "" {
		// Add URL
		if strings.HasPrefix(query, "http") || strings.HasPrefix(query, "magnet") {
			if err := StartTransfer(query); err != nil {
				Error(w, err)
				return
			}
		}

		// Search query
		results, err := search.Search(query)
		if err != nil {
			Error(w, err)
			return
		}
		// Truncate to 20 results max.
		if len(results) > 20 {
			results = results[0:19]
		}

		res.Results = results
		res.Query = query
		res.Results = results
	}

	transfers := ListTransfers()

	res.Transfers = transfers
	res.Section = "import"
	HTML(w, "import.html", res)
}

//
// Feeds
//

func feedPodcast(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	secret := ps.ByName("secret")
	if secret != feedsecret.Get() {
		logger.Errorf("auth: feed podcast invalid secret %q", secret)
		http.NotFound(w, r)
		return
	}

	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}

	// Find the most recently updated file.
	var updated time.Time
	for _, dl := range dls {
		for _, file := range dl.Files(false) {
			if !file.Viewable() {
				continue
			}
			modtime := file.Info.ModTime()
			if modtime.After(updated) {
				updated = modtime
			}
		}
	}

	baseurl := BaseURL(r)
	title := "Watcher - " + httpHost

	p := podcast.New(title, baseurl, title, &updated, &updated)
	p.AddAuthor(httpHost, "watcher@"+httpHost)
	p.AddImage(baseurl + "/logo.png")

	for _, dl := range dls {
		// Find viewable files
		var files []File
		for _, file := range dl.Files(false) {
			if !file.Viewable() {
				continue
			}
			files = append(files, file)
		}

		// Add each file as podcast item.
		for _, file := range files {
			typ := podcast.MP4
			switch file.Ext() {
			case "mp4":
				typ = podcast.MP4
			case "m4a":
				typ = podcast.MP4
			case "m4v":
				typ = podcast.M4V
			case "mp3":
				typ = podcast.MP3
			default:
				continue
			}

			pubDate := dl.Created
			stream, err := url.Parse(fmt.Sprintf("%s/feed/stream/%s/%s?secret=%s", baseurl, dl.ID, file.ID, feedsecret.Get()))
			if err != nil {
				Error(w, err)
				return
			}

			size := file.Info.Size()

			itemTitle := filepath.Base(file.ID)
			if len(files) == 1 {
				itemTitle = dl.ID
			}
			itemDesc := dl.ID

			item := podcast.Item{
				Title:       itemTitle,
				Description: itemDesc,
				PubDate:     &pubDate,
			}
			if file.Thumbnail() {
				item.AddImage(fmt.Sprintf("%s/feed/stream/%s/%s.thumbnail.png?secret=%s", baseurl, dl.ID, file.ID, feedsecret.Get()))
			}
			item.AddEnclosure(stream.String(), typ, size)
			if _, err := p.AddItem(item); err != nil {
				Error(w, err)
				return
			}
		}
	}

	// Sort
	sort.Slice(p.Items, func(i, j int) bool { return p.Items[i].PubDate.After(*p.Items[j].PubDate) })

	w.Header().Set("Content-Type", "application/xml")
	if err := p.Encode(w); err != nil {
		Error(w, err)
	}
}

func feedReset(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if err := feedsecret.Reset(); err != nil {
		Error(w, err)
		return
	}
	Redirect(w, r, "/?message=feedsecretreset")
}

func feedStream(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	secret := r.FormValue("secret")
	id := ps.ByName("id")
	filename := strings.TrimPrefix(ps.ByName("file"), "/")

	if secret != feedsecret.Get() {
		logger.Errorf("auth: feed stream invalid secret %q", secret)
		http.NotFound(w, r)
		return
	}

	dl, err := FindDownload(id)
	if err != nil {
		logger.Warnf("feed stream download %q: %s", id, err)
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(filename)
	if err != nil {
		logger.Warnf("feed stream file %q: %s", filename, err)
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", 7*86400))
	http.ServeFile(w, r, file.Path)
}

//
// API v1
//

func v1Status(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	// special auth, localhost only.
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip != "::1" && ip != "127.0.0.1" {
		http.NotFound(w, r)
		return
	}

	status := func() string {
		if tcer.Busy() {
			return "busy"
		}
		if dler.Busy() {
			return "busy"
		}
		return "idle"
	}()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%s\n", status)
}

func v1Downloads(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}

	var downloads []FriendDownload

	for _, dl := range dls {
		if !dl.Shared() {
			continue
		}
		downloads = append(downloads, FriendDownload{
			ID:   dl.ID,
			Size: dl.Size(),
		})
	}

	JSON(w, downloads)
}

func v1Files(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if !dl.Shared() {
		http.NotFound(w, r)
		return
	}

	var files []FriendFile
	for _, f := range dl.Files(false) {
		files = append(files, FriendFile{
			ID:   f.ID,
			Size: f.Info.Size(),
		})
	}
	JSON(w, files)
}

func v1Stream(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	dl, err := FindDownload(ps.ByName("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if !dl.Shared() {
		http.NotFound(w, r)
		return
	}

	file, err := dl.FindFile(strings.TrimPrefix(ps.ByName("file"), "/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	logger.Debugf("%s %s %q %q %q", r.RemoteAddr, ps.ByName("user"), r.Method, r.URL.Path, file.Path)
	http.ServeFile(w, r, file.Path)
}

//
// Assets
//

func staticAsset(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	serveAsset(w, r, ps.ByName("path"))
}

func logo(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	serveAsset(w, r, "/logo.png")
}

func serveAsset(w http.ResponseWriter, r *http.Request, filename string) {
	path := "static" + filename

	b, err := Asset(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fi, err := AssetInfo(path)
	if err != nil {
		Error(w, err)
		return
	}
	http.ServeContent(w, r, path, fi.ModTime(), bytes.NewReader(b))
}

//
// Helpers
//

func Redirect(w http.ResponseWriter, r *http.Request, format string, a ...interface{}) {
	location := httpPrefix
	location += fmt.Sprintf(format, a...)
	http.Redirect(w, r, location, http.StatusFound)
}

func Prefix(path string) string {
	return httpPrefix + path
}

//
// main
//

func main() {
	var err error
	cli.Parse(os.Args[1:])
	usage := func(msg string) {
		if msg != "" {
			fmt.Fprintf(os.Stderr, "ERROR: %s\n", msg)
		}
		fmt.Fprintf(os.Stderr, "Usage: %s [...]\n", os.Args[0])
		cli.PrintDefaults()
	}

	if showVersion {
		fmt.Printf("Watcher %s\n", version)
		os.Exit(0)
	}

	logtail, err = NewLogtailer(200 * 1024)
	if err != nil {
		panic(err)
	}

	// logger
	atomlevel := zap.NewAtomicLevel()
	l := zap.New(
		zapcore.NewCore(
			zapcore.NewConsoleEncoder(zap.NewProductionEncoderConfig()),
			zapcore.NewMultiWriteSyncer(zapcore.Lock(zapcore.AddSync(os.Stdout)), logtail),
			atomlevel,
		),
	)
	defer l.Sync()
	logger = l.Sugar()

	// debug logging
	if debug {
		atomlevel.SetLevel(zap.DebugLevel)
	}
	logger.Debugf("debug logging is enabled")

	// config
	config, err = NewConfig("config.json")
	if err != nil {
		logger.Fatal(err)
	}

	if httpHost == "" {
		usage("missing HTTP host")
		os.Exit(1)
	}

	// No trailing slash, please.
	httpPrefix = strings.TrimRight(httpPrefix, "/")

	// feed secret
	feedsecret = NewSecret(filepath.Join(downloadDir, ".feedsecret"))

	// http port
	httpIP, httpPort, err := net.SplitHostPort(httpAddr)
	if err != nil {
		usage("invalid --http-addr: " + err.Error())
	}

	// auth secret
	if reverseProxyAuthIP == "" {
		authsecret = NewSecret(filepath.Join(downloadDir, ".password"))
	}

	// transcoder
	tcer = transcoder.NewTranscoder()

	// downloader
	logger.Debugf("download directory is %q", downloadDir)

	dler, err = downloader.NewDownloader(&downloader.Config{
		DownloadDir: downloadDir,
		TorrentAddr: torrentListenAddr,
		Logger:      logger,
		Space: func() int64 {
			di, err := NewDiskInfo(downloadDir)
			if err != nil {
				logger.Fatal(err)
			}
			return di.Free()
		},
		TorrentRatio: config.Get().Ratio,
	})
	if err != nil {
		logger.Fatal(err)
	}

	// friends dir
	if !metadata {
		friendsDir = filepath.Join(downloadDir, ".friends")
		logger.Debugf("friends directory is %q", friendsDir)
		if err := os.MkdirAll(friendsDir, 0755); err != nil {
			logger.Fatal(err)
		}
	}

	//
	// Routes
	//
	r := httprouter.New()

	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.HandleMethodNotAllowed = false
	//r.HandleOPTIONS = false
	/*
		r.PanicHandler = func(w http.ResponseWriter, r *http.Request, recv interface{}) {
			logger.Errorf("DON'T PANIC: %s %s", recv, rtdebug.Stack())
			Error(w, fmt.Errorf("Internal Server Error"))
			return
		}
	*/

	// Downloads
	r.GET("/", Log(Auth(index, false)))
	r.GET(Prefix(""), Log(Auth(index, false)))
	r.GET(Prefix("/"), Log(Auth(library, false)))
	r.GET(Prefix("/logs"), Auth(logs, false))
	r.GET(Prefix("/downloads/list"), Auth(dlList, false))
	r.GET(Prefix("/downloads/files/:id"), Log(Auth(dlFiles, false)))
	r.GET(Prefix("/downloads/view/:id/*file"), Log(Auth(dlView, false)))
	r.GET(Prefix("/downloads/save/:id/*file"), Log(Auth(dlSave, false)))
	r.GET(Prefix("/downloads/stream/:id/*file"), Log(Auth(dlStream, false)))
	r.GET(Prefix("/downloads/remove/:id"), Log(Auth(dlRemove, false)))
	r.POST(Prefix("/downloads/share/:id"), Log(Auth(dlShare, false)))
	r.POST(Prefix("/downloads/unshare/:id"), Log(Auth(dlUnshare, false)))

	// Transfers
	r.GET(Prefix("/transfers/list"), Auth(transferList, false))
	r.GET(Prefix("/transfers/cancel/:id"), Log(Auth(transferCancel, false)))
	r.POST(Prefix("/transfers/start"), Log(Auth(transferStart, false)))
	r.POST(Prefix("/transfers/magnet"), Log(Auth(transferMagnet, false)))

	// Transcodings
	r.GET(Prefix("/transcode/start/:id/*file"), Log(Auth(transcodeStart, false)))
	r.GET(Prefix("/transcode/cancel/:id/*file"), Log(Auth(transcodeCancel, false)))

	// Friends
	r.GET(Prefix("/friends"), Log(Auth(friends, true)))
	r.POST(Prefix("/friends/add"), Log(Auth(friendAdd, true)))
	r.GET(Prefix("/friends/remove/:host"), Log(Auth(friendRemove, true)))
	r.POST(Prefix("/friends/download/:host/:dl"), Log(Auth(friendDownload, true)))

	// Feed
	r.GET(Prefix("/podcast/:secret"), Log(feedPodcast))
	r.GET(Prefix("/feed/stream/:id/*file"), Log(feedStream))
	r.GET(Prefix("/feed/reset"), Log(Auth(feedReset, false)))

	// Settings
	r.GET(Prefix("/settings"), Log(Auth(settings, false)))
	r.POST(Prefix("/settings"), Log(Auth(settings, false)))

	// Import
	r.GET(Prefix("/import"), Log(Auth(importHandler, false)))

	// API v1
	r.GET(Prefix("/v1/status"), Log(v1Status))
	r.GET(Prefix("/v1/downloads"), Log(Auth(v1Downloads, true)))
	r.GET(Prefix("/v1/downloads/files/:id"), Log(Auth(v1Files, true)))
	r.GET(Prefix("/v1/downloads/stream/:id/*file"), Log(Auth(v1Stream, true)))

	// Assets
	r.GET(Prefix("/static/*path"), Auth(staticAsset, false))
	r.GET(Prefix("/logo.png"), logo)

	//
	// Server
	//
	httpTimeout := 48 * time.Hour
	maxHeaderBytes := 10 * (1024 * 1024) // 10 MB

	// Plain text web server for use behind a reverse proxy.
	if !letsencrypt {
		httpd := &http.Server{
			Handler:        r,
			Addr:           net.JoinHostPort(httpIP, httpPort),
			WriteTimeout:   httpTimeout,
			ReadTimeout:    httpTimeout,
			MaxHeaderBytes: maxHeaderBytes,
		}
		hostport := net.JoinHostPort(httpHost, httpPort)
		if httpPort == "80" {
			hostport = httpHost
		}
		logger.Infof("Watcher version: %s %s", version, &url.URL{
			Scheme: "http",
			Host:   hostport,
			Path:   httpPrefix + "/",
		})
		if authsecret != nil {
			logger.Infof("Login credentials:  %s  /  %s", httpUsername, authsecret.Get())
		}
		logger.Fatal(httpd.ListenAndServe())
	}

	// Let's Encrypt TLS mode

	// http redirect to https
	go func() {
		redir := httprouter.New()
		redir.GET("/*path", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			r.URL.Scheme = "https"
			r.URL.Host = net.JoinHostPort(httpHost, httpPort)
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
		})

		httpd := &http.Server{
			Handler:        redir,
			Addr:           net.JoinHostPort(httpIP, "80"),
			WriteTimeout:   httpTimeout,
			ReadTimeout:    httpTimeout,
			MaxHeaderBytes: maxHeaderBytes,
		}
		if err := httpd.ListenAndServe(); err != nil {
			logger.Warnf("skipping redirect http port 80 to https port %s (%s)", httpPort, err)
		}
	}()

	// autocert
	m := autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(filepath.Join(downloadDir, ".autocert")),
		HostPolicy: autocert.HostWhitelist(httpHost, "www."+httpHost),
	}

	// TLS
	tlsConfig := tls.Config{
		GetCertificate: m.GetCertificate,
		NextProtos:     []string{"http/1.1"}, // TODO: investigate any HTTP 2 issues.
		Rand:           rand.Reader,
		PreferServerCipherSuites: true,
		MinVersion:               tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,

			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},
	}

	// Override default for TLS.
	if httpPort == "80" {
		httpPort = "443"
		httpAddr = net.JoinHostPort(httpIP, httpPort)
	}

	httpsd := &http.Server{
		Handler:        r,
		Addr:           httpAddr,
		WriteTimeout:   httpTimeout,
		ReadTimeout:    httpTimeout,
		MaxHeaderBytes: maxHeaderBytes,
	}

	// Enable TCP keep alives on the TLS connection.
	tcpListener, err := net.Listen("tcp", httpAddr)
	if err != nil {
		logger.Fatalf("listen failed: %s", err)
		return
	}
	tlsListener := tls.NewListener(tcpKeepAliveListener{tcpListener.(*net.TCPListener)}, &tlsConfig)

	hostport := net.JoinHostPort(httpHost, httpPort)
	if httpPort == "443" {
		hostport = httpHost
	}
	logger.Infof("Watcher version: %s %s", version, &url.URL{
		Scheme: "https",
		Host:   hostport,
		Path:   httpPrefix + "/",
	})
	logger.Infof("Login credentials:  %s  /  %s", httpUsername, authsecret.Get())
	logger.Fatal(httpsd.Serve(tlsListener))
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (l tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := l.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(10 * time.Minute)
	return tc, nil
}
