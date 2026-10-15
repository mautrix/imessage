package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func TestGroupContactReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name, membership, currentName, currentAvatar, wantedName, wantedAvatar string
		changed                                                                bool
	}{
		{"stale name with unchanged photo", "join", "+12025550123", "mxc://test/photo", "Contact Name", "mxc://test/photo", true},
		{"stale photo", "join", "Contact Name", "mxc://test/old", "Contact Name", "mxc://test/new", true},
		{"missing source photo retains room photo", "join", "+12025550123", "mxc://test/photo", "Contact Name", "", true},
		{"already current", "join", "Contact Name", "mxc://test/photo", "Contact Name", "mxc://test/photo", false},
		{"left member stays left", "leave", "old", "", "Contact Name", "mxc://test/photo", false},
		{"invited member stays invited", "invite", "old", "", "Contact Name", "mxc://test/photo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := map[string]any{"membership": tc.membership, "displayname": tc.currentName, "avatar_url": tc.currentAvatar, "custom.extension": map[string]any{"keep": true}}
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/_matrix/client/v3/rooms/!group:test/state/m.room.member/@ghost:test" {
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				if r.Method == "PUT" {
					writes++
					json.NewDecoder(r.Body).Decode(&content)
					json.NewEncoder(w).Encode(map[string]string{"event_id": "$updated"})
				} else {
					json.NewEncoder(w).Encode(content)
				}
			}))
			defer server.Close()
			client, err := mautrix.NewClient(server.URL, id.UserID("@ghost:test"), "test")
			if err != nil {
				t.Fatal(err)
			}
			changed, err := reconcileGroupMemberProfile(client, "!group:test", "@ghost:test", tc.wantedName, tc.wantedAvatar)
			if err != nil || changed != tc.changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if content["membership"] != tc.membership || !reflect.DeepEqual(content["custom.extension"], map[string]any{"keep": true}) {
				t.Fatal("changed membership or discarded extension")
			}
			if tc.changed {
				if writes != 1 || content["displayname"] != tc.wantedName {
					t.Fatal("name not reconciled")
				}
				if tc.wantedAvatar == "" && content["avatar_url"] != tc.currentAvatar {
					t.Fatal("removed existing photo")
				}
			} else if writes != 0 {
				t.Fatal("unnecessary write")
			}
			changed, err = reconcileGroupMemberProfile(client, "!group:test", "@ghost:test", tc.wantedName, tc.wantedAvatar)
			if err != nil || changed {
				t.Fatal("repeated sync was not idempotent")
			}
		})
	}
}

func TestGroupContactWriteFailureIsRetried(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			writes++
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]string{"errcode": "M_FORBIDDEN", "error": "test"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"membership": "join", "displayname": "old"})
	}))
	defer server.Close()
	client, _ := mautrix.NewClient(server.URL, "@ghost:test", "test")
	for i := 0; i < 2; i++ {
		changed, err := reconcileGroupMemberProfile(client, "!group:test", "@ghost:test", "Contact Name", "")
		if err == nil || changed {
			t.Fatal("failed write reported as success")
		}
	}
	if writes != 2 {
		t.Fatal("failed update not retried")
	}
}

func TestGroupContactRequiresMatchingReadback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$accepted"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"membership": "join", "displayname": "old"})
	}))
	defer server.Close()
	client, _ := mautrix.NewClient(server.URL, "@ghost:test", "test")
	changed, err := reconcileGroupMemberProfile(client, "!group:test", "@ghost:test", "Contact Name", "")
	if err == nil || changed {
		t.Fatal("an accepted write with stale readback was reported as verified")
	}
}
