package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"time"
)

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

	endpoint := fmt.Sprintf("https://%s/viewscreen/v1/downloads?friend=%s", f.ID, httpHost)

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
