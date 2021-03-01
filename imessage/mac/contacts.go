// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
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

package mac

//#cgo CFLAGS: -x objective-c -Wno-incompatible-pointer-types
//#cgo LDFLAGS: -framework Contacts -framework Foundation
//#include "meowContacts.h"
import "C"
import (
	"reflect"
	"unsafe"

	"go.mau.fi/mautrix-imessage/imessage"
)

type ContactStore struct {
	int       *C.CNContactStore
	HasAccess bool
}

var actualAuthCallback = make(chan bool)

//export meowAuthCallback
func meowAuthCallback(granted C.BOOL) {
	if granted {
		actualAuthCallback <- true
	} else {
		actualAuthCallback <- false
	}
}

func NewContactStore() *ContactStore {
	var cs ContactStore

	cs.int = C.meowCreateStore()
	switch C.meowCheckAuth() {
	case C.CNAuthorizationStatusNotDetermined:
		go C.meowRequestAuth(cs.int)
		cs.HasAccess = <-actualAuthCallback
	case C.CNAuthorizationStatusDenied:
		cs.HasAccess = false
	case C.CNAuthorizationStatusAuthorized:
		cs.HasAccess = true
	}
	return &cs
}

func gostring(s *C.NSString) string { return C.GoString(C.nsstring2cstring(s)) }

func cncontactToContact(ns *C.CNContact) *imessage.Contact {
	var contact imessage.Contact

	contact.FirstName = gostring(C.meowGetGivenNameFromContact(ns))
	contact.LastName = gostring(C.meowGetFamilyNameFromContact(ns))
	contact.Nickname = gostring(C.meowGetNicknameFromContact(ns))

	emails := C.meowGetEmailAddressesFromContact(ns)
	contact.Emails = make([]string, int(C.meowGetArrayLength(emails)))
	for i := range contact.Emails {
		contact.Emails[i] = gostring(C.meowGetEmailArrayItem(emails, C.ulong(i)))
	}

	phones := C.meowGetPhoneNumbersFromContact(ns)
	contact.Phones = make([]string, int(C.meowGetArrayLength(phones)))
	for i := range contact.Phones {
		contact.Phones[i] = gostring(C.meowGetPhoneArrayItem(phones, C.ulong(i)))
	}

	length := int(C.meowGetImageDataLengthFromContact(ns))
	if length > 0 {
		contact.Avatar = make([]byte, 0)
		header := (*reflect.SliceHeader)(unsafe.Pointer(&contact.Avatar))
		header.Len = length
		header.Cap = length
		header.Data = uintptr(C.meowGetImageDataFromContact(ns))
	}

	return &contact
}

func (cs *ContactStore) GetByEmail(email string) *imessage.Contact {
	cnContact := C.meowGetContactByEmail(cs.int, C.CString(email))
	return cncontactToContact(cnContact)
}

func (cs *ContactStore) GetByPhone(phone string) *imessage.Contact {
	cnContact := C.meowGetContactByPhone(cs.int, C.CString(phone))
	return cncontactToContact(cnContact)
}

func (imdb *Database) GetContactInfo(identifier string) (*imessage.Contact, error) {
	if len(identifier) == 0 {
		return nil, nil
	} else if identifier[0] == '+' {
		return imdb.contactStore.GetByPhone(identifier), nil
	} else {
		return imdb.contactStore.GetByEmail(identifier), nil
	}
}
