package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	//rtdebug "runtime/debug"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/tricklecloud/trickle/internal/downloader"
	"github.com/tricklecloud/trickle/internal/transcoder"

	"github.com/eduncan911/podcast"
	"github.com/julienschmidt/httprouter"
	"golang.org/x/crypto/acme/autocert"
)

var (
	// Flags
	cli         = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	downloadDir string

	httpAddr   string
	httpHost   string
	httpPrefix string

	letsencrypt bool
	metadata    bool
	backlink    string

	// set these based on download dir
	incomingDir string
	friendsDir  string

	// The version is set by the build command.
	version string

	// torrent
	torrentListenAddr string

	// reverse proxy authentication
	reverseProxyAuthIP     string
	reverseProxyAuthHeader string

	debug bool

	// feed secret
	feedSecret *Secret
	authSecret *Secret

	// transcoder
	tcer *transcoder.Transcoder

	// downloader
	dler *downloader.Downloader

	httpReadLimit int64 = 2 * (1024 * 1024) // 2 MB
)

func init() {
	cli.StringVar(&downloadDir, "download-dir", "/data", "download directory")
	cli.StringVar(&backlink, "backlink", "", "backlink (optional)")
	cli.StringVar(&httpAddr, "http-addr", ":80", "listen address")
	cli.StringVar(&httpHost, "http-host", "", "HTTP host")
	cli.StringVar(&httpPrefix, "http-prefix", "/trickle", "HTTP URL prefix (not supported yet)")
	cli.StringVar(&torrentListenAddr, "torrent-addr", ":61337", "listen address for torrent client")
	cli.StringVar(&reverseProxyAuthIP, "reverse-proxy-ip", "", "reverse proxy auth IP")
	cli.StringVar(&reverseProxyAuthHeader, "reverse-proxy-header", "X-Authenticated-User", "reverse proxy auth header")
	cli.BoolVar(&metadata, "metadata", false, "use metadata service")
	cli.BoolVar(&letsencrypt, "letsencrypt", false, "enable TLS using Let's Encrypt")
	cli.BoolVar(&debug, "debug", false, "debug mode")
}

// Index redirect
func index(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	redirect(w, r, "/")
}

//
// Downloads
//
func downloads(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	transfers := ListTransfers()

	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}

	res := NewResponse(r, ps)
	res.Transfers = transfers
	res.Downloads = dls
	res.Section = "downloads"
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
	res.Section = "downloads"
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

	log.Debugf("%s %s %q (%s)", r.RemoteAddr, ps.ByName("user"), r.Method, r.URL.Path, file.Path)
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
	log.Debugf("%s %s %q (%s)", r.RemoteAddr, ps.ByName("user"), r.Method, r.URL.Path, file.Path)
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
	redirect(w, r, "/?message=downloadremoved")
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

func transferStart(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	target := strings.TrimSpace(r.FormValue("target"))
	if target == "" {
		target = ps.ByName("target")
	}
	if err := StartTransfer(target); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/?message=transferstarted")
}

func transferCancel(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if err := CancelTransfer(ps.ByName("id")); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/?message=transfercanceled")
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

	log.Debugf("starting trancode %q", file.Path)

	if err := StartTranscode(file.Path); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/downloads/files/%s", dl.ID)
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

	log.Debugf("canceling trancode %q", file.Path)

	if err := CancelTranscode(file.Path); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/downloads/files/%s", dl.ID)
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
		redirect(w, r, "/friends?message=friendinvalidhost")
		return
	}

	if err := AddFriend(host); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/friends?message=friendadded")
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
	redirect(w, r, "/friends?message=friendremoved")
}

func friendDownload(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	host := ps.ByName("host")
	dl := ps.ByName("dl")

	f, err := FindFriend(host)
	if err != nil {
		Error(w, err)
		return
	}

	endpoint := fmt.Sprintf("https://%s/trickle/v1/downloads/files/%s?friend=%s", f.ID, dl, httpHost)

	if err := StartTransfer(endpoint); err != nil {
		Error(w, err)
		return
	}

	redirect(w, r, "/?message=transferstarted")
}

//
// Feeds
//

func feedPodcast(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	secret := ps.ByName("secret")
	if secret != feedSecret.Get() {
		log.Errorf("auth: feed invalid secret")
		http.NotFound(w, r)
		return
	}

	dls, err := ListDownloads()
	if err != nil {
		Error(w, err)
		return
	}

	var updated time.Time
	for _, dl := range dls {
		for _, file := range dl.Files() {
			if !file.Viewable() {
				continue
			}
			modtime := file.Info.ModTime()
			if modtime.After(updated) {
				updated = modtime
			}
		}
	}

	p := podcast.New("Trickle @ "+httpHost, "https://"+httpHost, "Trickle "+httpHost, &updated, &updated)
	p.AddAuthor(httpHost, "trickle@"+httpHost)
	p.AddImage("https://trickle.cloud/static/logo.png") // TODO: serve this directly.

	for _, dl := range dls {
		for _, file := range dl.Files() {
			if !file.Viewable() {
				continue
			}

			typ := podcast.MP4
			switch file.Ext() {
			case "mp4":
				typ = podcast.MP4
			case "m4v":
				typ = podcast.M4V
			case "mp3":
				typ = podcast.MP3
			default:
				continue
			}

			pubDate := file.Info.ModTime()
			stream := fmt.Sprintf("https://%s/trickle/feed/stream/%s/%s?secret=%s", httpHost, dl.ID, file.ID, feedSecret.Get())
			size := file.Info.Size()

			item := podcast.Item{
				Title:       fmt.Sprintf("%s (%s)", file.ID, dl.ID),
				Description: fmt.Sprintf("%s (%s)", file.ID, dl.ID),
				PubDate:     &pubDate,
			}
			item.AddEnclosure(stream, typ, size)
			if _, err := p.AddItem(item); err != nil {
				Error(w, err)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	if err := p.Encode(w); err != nil {
		Error(w, err)
	}
}

func feedReset(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	if err := feedSecret.Reset(); err != nil {
		Error(w, err)
		return
	}
	redirect(w, r, "/?message=feedsecretreset")
}

func feedStream(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	secret := r.FormValue("secret")
	if secret != feedSecret.Get() {
		log.Errorf("auth: feed invalid secret")
		http.NotFound(w, r)
		return
	}

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
	log.Debugf("feed stream: %s", r.URL)
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

	w.Header().Set("Content-Type", "text/plain")
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
	for _, f := range dl.Files() {
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
	log.Debugf("%s %s %q %q %q", r.RemoteAddr, ps.ByName("user"), r.Method, r.URL.Path, file.Path)
	http.ServeFile(w, r, file.Path)
}

//
// Assets
//

func staticAsset(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	path := "static" + ps.ByName("path")
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

func redirect(w http.ResponseWriter, r *http.Request, format string, a ...interface{}) {
	location := httpPrefix
	location += fmt.Sprintf(format, a...)
	http.Redirect(w, r, location, http.StatusFound)
}

func prefix(path string) string {
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

	if debug {
		log.SetLevel(log.DebugLevel)
	}
	log.Debugf("debug logging is enabled")

	if httpHost == "" {
		usage("missing HTTP host")
		os.Exit(1)
	}

	// No trailing slash, please.
	httpPrefix = strings.TrimRight(httpPrefix, "/")

	// feed secret
	feedSecret = NewSecret(filepath.Join(downloadDir, ".feedsecret"))

	// auth secret
	if reverseProxyAuthIP == "" || letsencrypt {
		authSecret = NewSecret(filepath.Join(downloadDir, ".authsecret"))
	}

	// transcoder
	tcer = transcoder.NewTranscoder()

	// downloader
	incomingDir = filepath.Join(downloadDir, ".incoming")
	log.Debugf("download directory is %q", downloadDir)
	log.Debugf("incoming directory is %q", incomingDir)

	dler, err = downloader.NewDownloader(downloadDir, incomingDir, torrentListenAddr, func() int64 {
		di, err := NewDiskInfo(downloadDir)
		if err != nil {
			log.Fatal(err)
		}
		return di.Free()
	})
	if err != nil {
		log.Fatal(err)
	}

	// friends dir
	if !metadata {
		friendsDir = filepath.Join(downloadDir, ".friends")
		log.Debugf("friends directory is %q", friendsDir)
		if err := os.MkdirAll(friendsDir, 0755); err != nil {
			log.Fatal(err)
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
			log.Errorf("DON'T PANIC: %s %s", recv, rtdebug.Stack())
			Error(w, fmt.Errorf("Internal Server Error"))
			return
		}
	*/

	// Downloads
	r.GET("/", Log(Auth(index, false)))
	r.GET(prefix(""), Log(Auth(index, false)))
	r.GET(prefix("/"), Log(Auth(downloads, false)))
	r.GET(prefix("/downloads/list"), Auth(dlList, false))
	r.GET(prefix("/downloads/files/:id"), Log(Auth(dlFiles, false)))
	r.GET(prefix("/downloads/view/:id/*file"), Log(Auth(dlView, false)))
	r.GET(prefix("/downloads/save/:id/*file"), Log(Auth(dlSave, false)))
	r.GET(prefix("/downloads/stream/:id/*file"), Log(Auth(dlStream, false)))
	r.GET(prefix("/downloads/remove/:id"), Log(Auth(dlRemove, false)))
	r.POST(prefix("/downloads/share/:id"), Log(Auth(dlShare, false)))
	r.POST(prefix("/downloads/unshare/:id"), Log(Auth(dlUnshare, false)))

	// Transfers
	r.GET(prefix("/transfers/list"), Auth(transferList, false))
	r.GET(prefix("/transfers/cancel/:id"), Log(Auth(transferCancel, false)))
	r.POST(prefix("/transfers/start"), Log(Auth(transferStart, false)))

	// Transcodings
	r.GET(prefix("/transcode/start/:id/*file"), Log(Auth(transcodeStart, false)))
	r.GET(prefix("/transcode/cancel/:id/*file"), Log(Auth(transcodeCancel, false)))

	// Friends
	r.GET(prefix("/friends"), Log(Auth(friends, true)))
	r.POST(prefix("/friends/add"), Log(Auth(friendAdd, true)))
	r.GET(prefix("/friends/remove/:host"), Log(Auth(friendRemove, true)))
	r.POST(prefix("/friends/download/:host/:dl"), Log(Auth(friendDownload, true)))

	// Feed
	r.GET(prefix("/podcast/:secret"), Log(feedPodcast))
	r.GET(prefix("/feed/stream/:id/*file"), Log(feedStream))
	r.GET(prefix("/feed/reset"), Log(Auth(feedReset, false)))

	// API v1
	r.GET(prefix("/v1/status"), Log(v1Status))
	r.GET(prefix("/v1/downloads"), Log(Auth(v1Downloads, true)))
	r.GET(prefix("/v1/downloads/files/:id"), Log(Auth(v1Files, true)))
	r.GET(prefix("/v1/downloads/stream/:id/*file"), Log(Auth(v1Stream, true)))

	// Assets
	r.GET(prefix("/static/*path"), Auth(staticAsset, false))

	//
	// Server
	//
	httpTimeout := 48 * time.Hour
	maxHeaderBytes := 10 * (1024 * 1024) // 10 MB

	// Plain text web server for use behind a reverse proxy.
	if !letsencrypt {
		p80 := &http.Server{
			Handler:        r,
			Addr:           httpAddr,
			WriteTimeout:   httpTimeout,
			ReadTimeout:    httpTimeout,
			MaxHeaderBytes: maxHeaderBytes,
		}
		log.Fatal(p80.ListenAndServe())
		log.Infof("http server %s http://%s%s", p80.Addr, httpHost, httpPrefix)
		return
	}

	// Let's Encrypt TLS mode

	// http redirect to https
	go func() {
		redir := httprouter.New()
		redir.GET("/", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			r.URL.Scheme = "https"
			r.URL.Host = httpHost
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
		})

		p80 := &http.Server{
			Handler:        redir,
			Addr:           ":80",
			WriteTimeout:   httpTimeout,
			ReadTimeout:    httpTimeout,
			MaxHeaderBytes: maxHeaderBytes,
		}
		if err := p80.ListenAndServe(); err != nil {
			log.Fatal(err)
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
	if httpAddr == ":80" {
		httpAddr = ":443"
	}

	p443 := &http.Server{
		Handler:        r,
		Addr:           httpAddr,
		WriteTimeout:   httpTimeout,
		ReadTimeout:    httpTimeout,
		MaxHeaderBytes: maxHeaderBytes,
	}

	// Enable TCP keep alives on the TLS connection.
	tcpListener, err := net.Listen("tcp", httpAddr)
	if err != nil {
		log.Fatalf("listen failed: %s", err)
		return
	}
	tlsListener := tls.NewListener(tcpKeepAliveListener{tcpListener.(*net.TCPListener)}, &tlsConfig)

	// TODO: display port if not on standard 443?
	log.Infof("Trickle URL: https://%s:%s@%s%s", "trickle", authSecret.Get(), httpHost, httpPrefix)
	log.Fatal(p443.Serve(tlsListener))
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
