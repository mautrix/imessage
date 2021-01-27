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

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"go.mau.fi/mautrix-imessage/imessage"
	_ "go.mau.fi/mautrix-imessage/imessage/mac"
)

func printMessage(db imessage.API, message imessage.Message) {
	var sender string
	contact := db.GetContactInfo(message.Sender.LocalID)
	if contact != nil {
		sender = contact.Name()
	}
	if len(sender) == 0 {
		sender = message.Sender.LocalID
	}
	if message.IsFromMe {
		if message.Chat.LocalID == message.Sender.LocalID {
			sender = fmt.Sprintf("you -> %s", sender)
		} else {
			sender = fmt.Sprintf("you -> %v", message.Chat)
		}
	} else if message.Chat.LocalID == message.Sender.LocalID {
		sender = fmt.Sprintf("%s -> you", sender)
	} else {
		sender = fmt.Sprintf("%s -> %v", sender, message.Chat)
	}
	fmt.Printf("%s <%s> %s\n", message.Time.Format("2006-01-02 15:04:05"), sender, strings.ReplaceAll(message.Text, "\n", "\\n"))
	if message.Attachment != nil {
		fmt.Println(message.Attachment)
	}
}

func read(r io.Reader) <-chan string {
	lines := make(chan string)
	go func() {
		defer close(lines)
		scan := bufio.NewScanner(r)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()
	return lines
}

func main() {
	db, err := imessage.NewAPI("mac")
	if err != nil {
		fmt.Println(err)
		return
	}
	//for _, contact := range db.Contacts {
	//	fmt.Println(contact.FirstName, contact.LastName, contact.Phones, contact.Emails, http.DetectContentType(contact.Avatar))
	//}
	messages, err := db.GetMessages("", imessage.AppleEpoch)
	if err != nil {
		fmt.Println(err)
		return
	}
	var lastChatID string
	for _, message := range messages {
		printMessage(db, message)
		lastChatID = message.ChatGUID
	}
	go func() {
		err := db.Start()
		fmt.Println("Watcher error:", err)
	}()
	fmt.Println("Listening to new messages")
	messageChan := db.MessageChan()
	stdin := read(os.Stdin)
	for {
		select {
		case message := <-messageChan:
			printMessage(db, message)
			lastChatID = message.ChatGUID
		case input := <-stdin:
			err = db.SendMessage(lastChatID, input)
			if err != nil {
				fmt.Println("Error sending message:", err)
			} else {
				fmt.Println("Message sent to", lastChatID)
			}
		}
	}
}
