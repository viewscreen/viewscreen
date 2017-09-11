package search

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"

	"github.com/PuerkitoBio/goquery"

	logger "github.com/Sirupsen/logrus"
)

type Result struct {
	Title    string
	Magnet   string
	Size     int64
	Seeders  int64
	Leechers int64
	Created  time.Time
}

func init() {
	logger.SetLevel(logger.DebugLevel)
}

func Search(query string) ([]Result, error) {
	rawurl := "https://thepiratebay.org/search/" + url.QueryEscape(query) + "/0/99/0"

	res, err := GET(rawurl)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	doc, err := goquery.NewDocumentFromReader(res.Body)
	if err != nil {
		return nil, err
	}

	var results []Result
	doc.Find("#searchResult").Find("tbody").Find("tr").Each(func(i int, s *goquery.Selection) {
		td1 := s.Find("td").Eq(1)
		td2 := s.Find("td").Eq(2)
		td3 := s.Find("td").Eq(3)

		// title
		var title string
		if link := td1.Find("a.detLink"); link != nil {
			title = link.AttrOr("title", "")
			title = strings.TrimSpace(title)
			title = strings.TrimPrefix(title, "Details for ")
		}
		if title == "" {
			logger.Debugf("result: no title found")
			return
		}

		// magnet
		magnet := td1.ChildrenFiltered("a").Eq(0).AttrOr("href", "")
		if magnet == "" {
			logger.Debugf("result: no magnet found")
			return
		}

		// size
		var size int64
		if desc := td1.Find("font.detDesc"); desc != nil {
			if parts := strings.Split(desc.Text(), ", "); len(parts) == 3 {
				if fields := strings.Fields(parts[1]); len(fields) == 3 {
					n, err := humanize.ParseBytes(fields[1] + " " + fields[2])
					if err == nil {
						size = int64(n)
					}
				}
			}
		}
		if size == 0 {
			logger.Debugf("result: no size found")
			return
		}

		// seeders
		var seeders int64
		seeders, _ = strconv.ParseInt(strings.TrimSpace(td2.Text()), 10, 64)
		if seeders == 0 {
			logger.Debugf("result: no seeders found")
			return
		}

		// leechers
		var leechers int64
		leechers, _ = strconv.ParseInt(strings.TrimSpace(td3.Text()), 10, 64)

		// created
		var created time.Time
		if desc := td1.Find("font.detDesc"); desc != nil {
			if parts := strings.Split(desc.Text(), ", "); len(parts) == 3 {
				if fields := strings.Fields(parts[0]); len(fields) == 3 {
					mdy := fields[1] + " " + fields[2]
					created, err = time.Parse(`01-02 2006`, mdy)
					if err != nil {
						logger.Debugf("result: parsing %q failed: %s", mdy, err)
					}
				}
			}
		}
		if created.IsZero() {
			// return
		}

		results = append(results, Result{
			Title:    title,
			Magnet:   magnet,
			Size:     size,
			Seeders:  seeders,
			Leechers: leechers,
			Created:  created,
		})
	})

	return results, nil
}

func GET(rawurl string) (*http.Response, error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
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
