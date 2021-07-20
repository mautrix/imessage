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
	"fmt"
	"reflect"
	"runtime"
	"unsafe"

	"go.mau.fi/mautrix-imessage/imessage"
)

type ContactStore struct {
	int       *C.CNContactStore
	HasAccess bool
}

var actualAuthCallback = make(chan error)

//export meowAuthCallback
func meowAuthCallback(granted C.int, errorDescription, errorReason *C.char) {
	if granted == 1 {
		actualAuthCallback <- nil
	} else if errorDescription != nil {
		actualAuthCallback <- fmt.Errorf("%s. %s", C.GoString(errorDescription), C.GoString(errorReason))
	} else {
		actualAuthCallback <- fmt.Errorf("unexpected granted status: %v", granted)
	}
}

func NewContactStore() *ContactStore {
	return &ContactStore{
		int: C.meowCreateStore(),
	}
}

func (cs *ContactStore) RequestAccess() error {
	switch C.meowCheckAuth() {
	case C.CNAuthorizationStatusNotDetermined:
		go C.meowRequestAuth(cs.int)
		err := <-actualAuthCallback
		cs.HasAccess = err == nil
		return err
	case C.CNAuthorizationStatusDenied:
		cs.HasAccess = false
	case C.CNAuthorizationStatusAuthorized:
		cs.HasAccess = true
	}
	return nil
}

func gostring(s *C.NSString) string { return C.GoString(C.nsstring2cstring(s)) }

func cncontactToContact(ns *C.CNContact) *imessage.Contact {
	if ns == nil {
		return nil
	}

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

	if length := int(C.meowGetImageDataLengthFromContact(ns)); length > 0 {
		avatarData := make([]byte, 0)
		header := (*reflect.SliceHeader)(unsafe.Pointer(&avatarData))
		header.Len = length
		header.Cap = length
		header.Data = uintptr(C.meowGetImageDataFromContact(ns))
		// The avatar data pointer comes from Objective-C, so we copy the data into a Go-managed array here.
		contact.Avatar = make([]byte, len(avatarData))
		copy(contact.Avatar, avatarData)
	}

	return &contact
}

func (mac *macOSDatabase) GetContactInfo(identifier string) (*imessage.Contact, error) {
	if !mac.contactStore.HasAccess || len(identifier) == 0 {
		return nil, nil
	} else if len(identifier) == 0 {
		return nil, fmt.Errorf("can't get contact info of empty identifier")
	}

	// Locking the OS thread seems to prevent random SIGSEGV's from the NSAutoreleasePool being drained.
	// I don't know why, but it's probably something to do with memory management and/or threading.
	runtime.LockOSThread()
	// This makes a NSAutoreleasePool, which enables Objective-C's memory management.
	pool := C.meowMakePool()

	var cnContact *C.CNContact
	if identifier[0] == '+' {
		cnContact = C.meowGetContactByPhone(mac.contactStore.int, C.CString(identifier))
	} else {
		cnContact = C.meowGetContactByEmail(mac.contactStore.int, C.CString(identifier))
	}
	goContact := cncontactToContact(cnContact)

	// Release all memory Obj-C stuff was using and unlock the OS thread.
	C.meowReleasePool(pool)
	runtime.UnlockOSThread()

	return goContact, nil
}
