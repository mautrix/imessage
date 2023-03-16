// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2023 Tulir Asokan, Sumner Evans
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	log "maunium.net/go/maulogger/v2"
)

const SegmentURL = "https://api.segment.io/v1/track"

type SegmentClient struct {
	key    string
	userID string
	log    log.Logger
	client http.Client
}

var Segment SegmentClient

func (sc *SegmentClient) trackSync(event string, properties map[string]any, userIDOverride string) error {
	var buf bytes.Buffer
	if userIDOverride == "" {
		userIDOverride = sc.userID
	}
	err := json.NewEncoder(&buf).Encode(map[string]interface{}{
		"userId":     userIDOverride,
		"event":      event,
		"properties": properties,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", SegmentURL, &buf)
	if err != nil {
		return err
	}
	req.SetBasicAuth(sc.key, "")
	resp, err := sc.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}
	return nil
}

func (sc *SegmentClient) IsEnabled() bool {
	return len(sc.key) > 0
}

func (sc *SegmentClient) Track(event string, properties ...map[string]any) {
	sc.TrackUser(event, "", properties...)
}

func (sc *SegmentClient) TrackUser(event, userID string, properties ...map[string]any) {
	if !sc.IsEnabled() {
		return
	} else if len(properties) > 1 {
		panic("Track should be called with at most one property map")
	}

	go func() {
		props := map[string]interface{}{}
		if len(properties) > 0 {
			props = properties[0]
		}
		props["bridge"] = "imessagecloud"
		err := sc.trackSync(event, props, userID)
		if err != nil {
			sc.log.Errorfln("Error tracking %s: %v", event, err)
		} else {
			sc.log.Debugln("Tracked", event)
		}
	}()
}

func (br *IMBridge) initSegment() {
	Segment.log = br.Log.Sub("Segment")
	Segment.key = br.Config.Segment.Key
	Segment.userID = br.Config.Segment.UserID
	if Segment.userID == "" {
		Segment.userID = br.Config.Bridge.User.String()
	}
	if Segment.IsEnabled() {
		Segment.log.Infoln("Segment metrics are enabled")
		if Segment.userID != "" {
			Segment.log.Infoln("Overriding Segment user_id with %v", Segment.userID)
		}
	}
}
