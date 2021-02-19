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

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.mau.fi/mautrix-imessage/imessage"
)

var phoneNumberCleaner = strings.NewReplacer("(", "", ")", "", " ", "", "-", "")

const contactInfoQuery = `
SELECT ZABCDRECORD.Z_PK, ZABCDPHONENUMBER.ZFULLNUMBER, ZABCDEMAILADDRESS.ZADDRESS, ZABCDRECORD.ZIMAGEDATA,
       ZABCDRECORD.ZIMAGEREFERENCE, ZABCDRECORD.ZFIRSTNAME, ZABCDRECORD.ZLASTNAME
FROM ZABCDRECORD
LEFT JOIN ZABCDPHONENUMBER ON ZABCDRECORD.Z_PK = ZABCDPHONENUMBER.ZOWNER
LEFT JOIN ZABCDEMAILADDRESS ON ZABCDRECORD.Z_PK = ZABCDEMAILADDRESS.ZOWNER
`

func columnExists(db *sql.DB, table, column string) bool {
	row := db.QueryRow(fmt.Sprintf(`SELECT name FROM pragma_table_info("%s") WHERE name=$1;`, table), column)
	var name string
	_ = row.Scan(&name)
	return name == column
}

func (imdb *Database) loadAddressBook() error {
	path, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	addressBookDir := filepath.Join(path, "Library", "Application Support", "AddressBook", "Sources")
	var addressDatabases []string
	err = filepath.Walk(addressBookDir, func(path string, info os.FileInfo, err error) error {
		name := info.Name()
		if !info.IsDir() && strings.HasPrefix(name, "Address") && strings.HasSuffix(name, ".abcddb") {
			addressDatabases = append(addressDatabases, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk address book directory: %w", err)
	}
	imdb.Contacts = make(map[string]*imessage.Contact)
	for _, dbPath := range addressDatabases {
		currentID := filepath.Base(filepath.Dir(dbPath))
		var db *sql.DB
		db, err = sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro", dbPath))
		if err != nil {
			return fmt.Errorf("failed to open address book %s database: %w", currentID, err)
		}
		var res *sql.Rows
		if columnExists(db, "ZABCDRECORD", "ZIMAGEDATA") {
			patchedContactInfoQuery := strings.ReplaceAll(contactInfoQuery, "ZABCDRECORD.ZIMAGEDATA", "null")
			res, err = db.Query(patchedContactInfoQuery)
		} else {
			res, err = db.Query(contactInfoQuery)
		}
		if err != nil {
			return fmt.Errorf("error querying address book %s database: %w", currentID, err)
		}
		contacts := make(map[int]*imessage.Contact)
		for res.Next() {
			var id int
			var number, email, avatarReference, firstName, lastName sql.NullString
			var avatar []byte
			err = res.Scan(&id, &number, &email, &avatar, &avatarReference, &firstName, &lastName)
			if err != nil {
				return fmt.Errorf("error scanning row in %s: %w", currentID, err)
			}
			contact, ok := contacts[id]
			if !ok {
				contact = &imessage.Contact{FirstName: firstName.String, LastName: lastName.String, Avatar: avatar, AvatarRef: avatarReference.String}
				contacts[id] = contact
			}
			if number.Valid && len(number.String) > 0 {
				numberStr := phoneNumberCleaner.Replace(number.String)
				_, phoneExists := imdb.Contacts[numberStr]
				if !phoneExists {
					contact.Phones = append(contact.Phones, numberStr)
					imdb.Contacts[numberStr] = contact
				}
			}
			if email.Valid && len(email.String) > 0 {
				_, emailExists := imdb.Contacts[email.String]
				if !emailExists {
					contact.Emails = append(contact.Emails, email.String)
					imdb.Contacts[email.String] = contact
				}
			}
		}
	}
	return nil
}

func (imdb *Database) GetContactInfo(identifier string) (*imessage.Contact, error) {
	contact, ok := imdb.Contacts[identifier]
	if !ok {
		return nil, nil
	}
	return contact, nil
}
