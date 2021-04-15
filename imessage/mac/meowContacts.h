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

const char* nsstring2cstring(NSString* s);

extern void meowAuthCallback(int granted, char* errorDescription, char* errorReason);
CNAuthorizationStatus meowCheckAuth();
CNContactStore* meowCreateStore();
void meowRequestAuth(CNContactStore* store);

CNContact* meowGetContactByEmail(CNContactStore* store, char* emailAddressC);
CNContact* meowGetContactByPhone(CNContactStore* store, char* phoneNumberC);

NSString* meowGetGivenNameFromContact(CNContact* contact);
NSString* meowGetFamilyNameFromContact(CNContact* contact);
NSString* meowGetNicknameFromContact(CNContact* contact);

const void* meowGetImageDataFromContact(CNContact* contact);
unsigned long meowGetImageDataLengthFromContact(CNContact* contact);

NSArray<CNLabeledValue<NSString*>*>* meowGetEmailAddressesFromContact(CNContact* contact);
NSArray<CNLabeledValue<CNPhoneNumber*>*>* meowGetPhoneNumbersFromContact(CNContact* contact);
NSString* meowGetPhoneArrayItem(NSArray<CNLabeledValue<CNPhoneNumber*>*>* arr, unsigned long i);
NSString* meowGetEmailArrayItem(NSArray<CNLabeledValue<NSString*>*>* arr, unsigned long i);
unsigned long meowGetArrayLength(NSArray* arr);

NSAutoreleasePool* meowMakePool();
void meowReleasePool(NSAutoreleasePool* pool);
