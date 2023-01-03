//go:build !darwin || ios

package mac_nosip

import (
	"errors"

	"go.mau.fi/mautrix-imessage/imessage"
	log "maunium.net/go/maulogger/v2"
)

func setupContactProxy(log log.Logger) (imessage.ContactAPI, error) {
	return nil, errors.New("can't use native contact access: not compiled for a Mac")
}
