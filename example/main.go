package main

import (
	"fmt"
	"net/mail"
	"os"

	"github.com/saranrapjs/maildotapp"
)

func checkErr(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	mboxes, err := maildotapp.NewMailboxes()
	checkErr(err)
	var mbox maildotapp.Mailbox
	if os.Getenv("ACCOUNT_NAME") != "" {
		mbox, err = mboxes.Mailbox(os.Getenv("ACCOUNT_NAME"), maildotapp.Inbox)
		checkErr(err)
	}
	query := mboxes.Query(maildotapp.MailboxQuery{
		Mailbox: mbox,
		BatchResults: 10,
	})
	messages, err := query()
	checkErr(err)
	r, err := messages[0].Open()
	checkErr(err)
	email, err := mail.ReadMessage(r)
	checkErr(err)
	fmt.Println(email.Header.Get("Subject"))
}
