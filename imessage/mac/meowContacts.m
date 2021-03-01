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
#import <Contacts/Contacts.h>
#include "meowContacts.h"

const char* nsstring2cstring(NSString *s) {
    if (s == NULL) { return NULL; }

    const char *cstr = [s UTF8String];
    return cstr;
}

CNAuthorizationStatus meowCheckAuth() {
	return [CNContactStore authorizationStatusForEntityType:CNEntityTypeContacts];
}

CNContactStore* meowCreateStore(void) {
	CNContactStore *store = [[CNContactStore alloc] init];
	return store;
}

void meowRequestAuth(CNContactStore *store) {
	[store requestAccessForEntityType:CNEntityTypeContacts completionHandler:^(BOOL granted, NSError * _Nullable error) {
		meowAuthCallback(granted == YES);
	}];
}

CNContact* meowGetContactByPredicate(CNContactStore *store, NSPredicate* predicate) {
	NSArray* keysToFetch = @[
		CNContactGivenNameKey, CNContactFamilyNameKey, CNContactNicknameKey,
		CNContactEmailAddressesKey, CNContactPhoneNumbersKey, CNContactImageDataKey
	];
	NSError *error;
	NSArray *contacts = [store unifiedContactsMatchingPredicate:predicate keysToFetch:keysToFetch error:&error];
	if (contacts == NULL || contacts.count == 0) {
		return NULL;
	}
	return [contacts objectAtIndex:0];
}

CNContact* meowGetContactByEmail(CNContactStore *store, char *emailAddressC) {
	NSString* emailAddressNS = [NSString stringWithUTF8String:emailAddressC];
	NSPredicate* predicate = [CNContact predicateForContactsMatchingEmailAddress:emailAddressNS];
	return meowGetContactByPredicate(store, predicate);
}

CNContact* meowGetContactByPhone(CNContactStore *store, char *phoneNumberC) {
	NSString* phoneNumberNS = [NSString stringWithUTF8String:phoneNumberC];
	CNPhoneNumber* phoneNumber = [CNPhoneNumber phoneNumberWithStringValue:phoneNumberNS];
	NSPredicate* predicate = [CNContact predicateForContactsMatchingPhoneNumber:phoneNumber];
	return meowGetContactByPredicate(store, predicate);
}

NSString* meowGetGivenNameFromContact(CNContact* contact)  { return contact.givenName; }
NSString* meowGetFamilyNameFromContact(CNContact* contact) { return contact.familyName; }
NSString* meowGetNicknameFromContact(CNContact* contact)   { return contact.nickname; }

const void* meowGetImageDataFromContact(CNContact* contact) { return contact.imageData.bytes; }
unsigned long meowGetImageDataLengthFromContact(CNContact* contact) {
	if (contact.imageData == NULL) {
		return 0;
	}
	return contact.imageData.length;
}

NSArray<CNLabeledValue<NSString*>*>* meowGetEmailAddressesFromContact(CNContact* contact)    { return contact.emailAddresses; }
NSArray<CNLabeledValue<CNPhoneNumber*>*>* meowGetPhoneNumbersFromContact(CNContact* contact) { return contact.phoneNumbers; }
NSString* meowGetPhoneArrayItem(NSArray<CNLabeledValue<CNPhoneNumber*>*>* arr, unsigned long i) { return [arr objectAtIndex:i].value.stringValue; }
NSString* meowGetEmailArrayItem(NSArray<CNLabeledValue<NSString*>*>* arr, unsigned long i)      { return [arr objectAtIndex:i].value; }
unsigned long meowGetArrayLength(NSArray* arr) {
    if (arr == NULL) {
		return 0;
    }
    return arr.count;
}
