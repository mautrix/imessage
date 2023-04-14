//go:build !darwin || ios

package mac_nosip

import (
	"errors"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
)

func setupContactProxy(log log.Logger) (imessage.ContactAPI, error) {
	return nil, errors.New("can't use native contact access: not compiled for a Mac")
}

func setupChatInfoProxy(log log.Logger) (imessage.ChatInfoAPI, error) {
	return nil, errors.New("can't use native chat info access: not compiled for a Mac")
}
