package bluebubbles

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-imessage/imessage"
)

type contactTestBridge struct {
	imessage.Bridge
	cfg imessage.PlatformConfig
}

func (b *contactTestBridge) GetConnectorConfig() *imessage.PlatformConfig { return &b.cfg }

func TestContactRefreshUpdatesProfilesAndRetainsCacheOnFailure(t *testing.T) {
	var generation atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/contact" || r.URL.Query().Get("extraProperties") != "avatar" {
			t.Error("contact refresh must request contact photos")
		}
		if generation.Load() == 3 {
			_, _ = w.Write([]byte("invalid response"))
			return
		}
		contacts := []Contact{}
		if generation.Load() < 2 {
			c := Contact{ID: "contact-id", FirstName: "Before", Emails: []Email{{Address: "test@example.org"}}}
			if generation.Load() == 1 {
				photo := "cGhvdG8="
				c.FirstName, c.Avatar = "After", &photo
			}
			contacts = append(contacts, c)
		}
		_ = json.NewEncoder(w).Encode(ContactResponse{Status: 200, Data: contacts})
	}))
	defer server.Close()
	bb := &blueBubbles{bridge: &contactTestBridge{cfg: imessage.PlatformConfig{BlueBubblesURL: server.URL}}, log: zerolog.Nop()}
	if err := bb.RefreshContactList(); err != nil {
		t.Fatal(err)
	}
	contact, err := bb.GetContactInfo("test@example.org")
	if err != nil || contact.Name() != "Before" || contact.Avatar != nil {
		t.Fatal("contact without a photo must still load")
	}
	generation.Store(1)
	if err := bb.RefreshContactList(); err != nil {
		t.Fatal(err)
	}
	contact, err = bb.GetContactInfo("test@example.org")
	if err != nil || contact.Name() != "After" || string(contact.Avatar) != "photo" {
		t.Fatal("refresh did not update the name and photo")
	}
	for _, failure := range []int32{2, 3} {
		generation.Store(failure)
		if err := bb.RefreshContactList(); err == nil {
			t.Fatal("empty or invalid response must report failure with an existing cache")
		}
		contact, err = bb.GetContactInfo("test@example.org")
		if err != nil || contact.Name() != "After" || string(contact.Avatar) != "photo" {
			t.Fatal("temporary contact failure discarded the known profile")
		}
	}
	generation.Store(1)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := bb.RefreshContactList(); err != nil {
					t.Error(err)
				}
				if _, err := bb.GetContactInfo("test@example.org"); err != nil {
					t.Error(err)
				}
				_, _ = bb.GetContactList()
				_, _ = bb.SearchContactList("After")
			}
		}()
	}
	wg.Wait()
}
