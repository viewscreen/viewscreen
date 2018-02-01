package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/viewscreen/viewscreen/internal/downloader"
	"github.com/viewscreen/viewscreen/internal/search"

	humanize "github.com/dustin/go-humanize"
	httprouter "github.com/julienschmidt/httprouter"
)

type Response struct {
	Template string

	HTTPHost   string
	Error      string
	Backlink   string
	User       string
	FeedSecret string

	DiskInfo *DiskInfo
	Request  *http.Request

	Section string

	Friends []Friend

	Download  Download
	Downloads []Download
	Library   []Download

	File File

	Transfer         downloader.Transfer
	Transfers        []downloader.Transfer
	TransfersPending []downloader.Transfer

	Sort  string
	Query string

	Results []search.Result

	Version string

	Config *Config
}

var (
	funcMap = template.FuncMap{
		"safe": func(s string) template.HTML {
			return template.HTML(s)
		},
		"dlexists": func(id string) bool {
			dl := Download{ID: id}
			if dl.Downloading() {
				return true
			}
			_, err := FindDownload(id)
			return err == nil
		},
		"percent": func(a, b int64) float64 {
			return (float64(a) / float64(b)) * 100
		},
		"bytes": func(n int64) string {
			return fmt.Sprintf("%.2f GB", float64(n)/1024/1024/1024)
			// return humanize.Bytes(uint64(n))
		},
		"time": humanize.Time,
		"truncate": func(s string, n int) string {
			if len(s) > n {
				s = s[:n-3] + "..."
			}
			return s
		},
	}
	errorPageHTML = `
        <html>
            <head>
                <title>Error</title>
            </head>
            <body>
                <h2 style="color: orangered;">ERROR</h2>
                <h3><a href="/viewscreen/logs">Logs</a></h3>
            </body>
        </html>
    `
)

func NewResponse(r *http.Request, ps httprouter.Params) *Response {
	di, err := NewDiskInfo(downloadDir)
	if err != nil {
		panic(err)
	}
	return &Response{
		Request:    r,
		User:       ps.ByName("user"),
		HTTPHost:   httpHost,
		DiskInfo:   di,
		FeedSecret: feedsecret.Get(),
		Version:    version,
		Backlink:   backlink,
		Config:     config,
	}
}

func Error(w http.ResponseWriter, err error) {
	logger.Error(err)

	w.WriteHeader(http.StatusInternalServerError)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, errorPageHTML)
}

func XML(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/xml")
	enc := xml.NewEncoder(w)
	enc.Indent("", "    ")
	if err := enc.Encode(data); err != nil {
		logger.Error(err)
	}
}

func JSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	if err := enc.Encode(data); err != nil {
		logger.Error(err)
	}
}

func HTML(w http.ResponseWriter, target string, data interface{}) {
	t := template.New(target)
	t.Funcs(funcMap)
	for _, filename := range AssetNames() {
		if !strings.HasPrefix(filename, "templates/") {
			continue
		}
		name := strings.TrimPrefix(filename, "templates/")
		b, err := Asset(filename)
		if err != nil {
			Error(w, err)
			return
		}

		var tmpl *template.Template
		if name == t.Name() {
			tmpl = t
		} else {
			tmpl = t.New(name)
		}
		if _, err := tmpl.Parse(string(b)); err != nil {
			Error(w, err)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html")
	if err := t.Execute(w, data); err != nil {
		Error(w, err)
		return
	}
}

func Log(h httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		start := time.Now()
		h(w, r, ps)
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		xff := r.Header.Get("X-Forwarded-For")
		xrealip := r.Header.Get("X-Real-IP")
		rang := r.Header.Get("Range")

		logger.Infof("%q %q %q %q %s %q %d ms", ip, xff, xrealip, rang, r.Method, r.RequestURI, int64(time.Since(start)/time.Millisecond))
	}
}

func Auth(h httprouter.Handle, friends bool) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		failed := true
		user := ""

		clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		// Auth Method: Friend DNS (only enabled on some routes).
		if friends && r.FormValue("friend") != "" {
			func() {
				host := r.FormValue("friend")
				logger.Debugf("auth: friend host %q", host)

				if host == "" {
					return
				}

				// Must be on friends list.
				friends, err := ListFriends()
				if err != nil {
					logger.Error(err)
					return
				}
				friendly := false
				for _, friend := range friends {
					if host == friend.ID {
						friendly = true
					}
				}
				if !friendly {
					return
				}

				// Reverse IP address lookup must match claimed host.
				if addrs, err := net.LookupHost(host); err == nil {
					for _, addr := range addrs {
						logger.Debugf("auth: friend match on client %q", addr)
						if addr == clientIP {
							failed = false
							user = host
							return
						}

						if clientIP == reverseProxyAuthIP {
							xff := r.Header.Get("X-Forwarded-For")
							xrealip := r.Header.Get("X-Real-IP")
							if strings.Contains(xff, addr) || strings.Contains(xrealip, addr) { // TODO: split xff into ip:port parts
								logger.Debugf("auth: friend match addr %q in xff %q", addr, xff)
								failed = false
								user = host
								return
							}
						}
					}
				}
				return
			}()

		} else if reverseProxyAuthIP == "" {
			// Auth Method: Basic Auth (if we're not behind a reverse proxy, use basic auth)
			login, password, _ := r.BasicAuth()
			if login == httpUsername && password == authsecret.Get() {
				failed = false
				user = login
			} else {
				w.Header().Set("WWW-Authenticate", `Basic realm="Login Required"`)
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
		} else {
			// Method: Reverse Proxy (if we're behind a reverse proxy, trust it.)
			if clientIP == reverseProxyAuthIP {
				if u := r.Header.Get(reverseProxyAuthHeader); u != "" {
					failed = false
					user = u
				}
			}
		}

		if failed {
			logger.Errorf("auth failed: client %q", clientIP)
			if backlink != "" {
				http.Redirect(w, r, backlink, http.StatusFound)
				return
			}
			http.NotFound(w, r)
			return
		}

		// Add "user" to params.
		ps = append(ps, httprouter.Param{Key: "user", Value: user})
		h(w, r, ps)
	}
}

func BaseURL(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = r.Method
	}
	if scheme != "http" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s", scheme, httpHost, httpPrefix)
}
