package maildotapp

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const getMailboxAccountUUIDs = `
tell application "Mail"
	set theAccounts to every account
	repeat with anAccount in theAccounts
		set theMailboxes to mailboxes of anAccount
		repeat with aMailbox in theMailboxes
			set accountId to id of anAccount
			set accountName to name of anAccount
			set mailboxName to name of aMailbox
			set csvData to accountId & "," & accountName & "," & mailboxName & return
			log csvData
		end repeat
	end repeat
end tell
`

// Account represents a Mail account that contains
// one or more Mailboxes.
type Account struct {
	Name string
	UUID string
}

// Mailbox describes a specific folder within an
// Account in Mail.app.
type Mailbox struct {
	Name    string
	Account *Account
}

func (m Mailbox) URL() string {
	return fmt.Sprintf("imap://%s/%s", m.Account.UUID, m.Name)
}

func (m Mailbox) IsEmpty() bool {
	return m.Account == nil || m.Name == ""
}

type MailboxQuery struct {
	Mailbox      Mailbox
	BatchResults int
}

// Mail.app (or IMAP? I'm not sure) uses "INBOX"
// as the standard name for account inboxes.
const Inbox = "INBOX"

// Gets a specific Mailbox, given an account name and
// mailbox name.
func (m Mailboxes) Mailbox(account, name string) (Mailbox, error) {
	if mbx, ok := m.byAccountName[account][name]; ok {
		return mbx, nil
	}
	return Mailbox{}, errors.New(fmt.Sprintf("couldn't find mailbox '%s' for account '%s'", name, account))
}

func (m Mailboxes) Query(mq MailboxQuery) func() ([]Message, error) {
	query := getMessages
	if !mq.Mailbox.IsEmpty() {
		query += "\nWHERE mbx.url = ?"
	}
	query += "\nORDER BY m.date_received DESC"
	var batchCount int
	if mq.BatchResults > 0 {
		query += "\nLIMIT ? OFFSET ?"
	}
	return func() ([]Message, error) {
		var variables []interface{}
		var msgs []Message
		if !mq.Mailbox.IsEmpty() {
			variables = append(variables, mq.Mailbox.URL())
		}
		if mq.BatchResults > 0 {
			variables = append(variables, mq.BatchResults, batchCount*mq.BatchResults)
		}
		rows, err := m.db.Query(query, variables...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var ROWID, u string
			if err := rows.Scan(&ROWID, &u); err != nil {
				return nil, err
			}
			MailboxURL, _ := url.Parse(u)
			relativePath := MailboxURL.Host + MailboxURL.Path
			basePath, ok := m.url2path[relativePath]
			if !ok {
				return nil, fmt.Errorf("unmatched mailbox path: %s", relativePath)
			}
			msgs = append(msgs, Message{
				pathWithoutExtension: path.Join(basePath, emlPathFromROWID(ROWID)),
			})
		}
		return msgs, nil
	}
}

// Retrieves the account-specific UUID's for each
// mailbox name from AppleScript.
func getMailboxes() ([]Mailbox, error) {
	lines := strings.Split(getMailboxAccountUUIDs, "\n")
	var args []string
	for _, line := range lines {
		if len(line) > 0 {
			args = append(args, "-e", line)
		}
	}
	cmd := exec.Command("osascript", args...)
	var buf bytes.Buffer
	cmd.Stderr = &buf
	reader := csv.NewReader(&buf)
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	accts := make(map[string]*Account)
	var mailboxes []Mailbox
	for _, record := range records {
		accountUUID := record[0]
		accountName := record[1]
		mailboxName := record[2]
		var account *Account
		if acct, ok := accts[accountUUID]; ok {
			account = acct
		} else {
			account = &Account{
				Name: accountName,
				UUID: accountUUID,
			}
		}
		mailbox := Mailbox{
			Name:    mailboxName,
			Account: account,
		}
		mailboxes = append(mailboxes, mailbox)
	}
	return mailboxes, nil
}

const getMessages = `
SELECT
	m.ROWID as id,
	mbx.url as url
FROM
	messages m
LEFT JOIN
	mailboxes mbx
ON
	m.mailbox = mbx.ROWID
`

var homeDir string

func init() {
	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	homeDir = userHomeDir
}

type Mailboxes struct {
	byAccountName map[string]map[string]Mailbox
	url2path      map[string]string
	db            *sql.DB
}

func wrapMaybePermissionsError(err error) error {
	if strings.HasSuffix(err.Error(), "operation not permitted") {
		return fmt.Errorf("Querying Mail.app messages requires full-disk access permissions.\nYou can grant these permissions in System Preferences > Privacy & Security.\n\nOriginal error:\n%w", err)
	}
	return err
}

func NewMailboxes() (Mailboxes, error) {
	m := Mailboxes{
		byAccountName: make(map[string]map[string]Mailbox),
	}
	mboxes, err := getMailboxes()
	if err != nil {
		return m, err
	}
	for _, mbox := range mboxes {
		if m.byAccountName[mbox.Account.Name] == nil {
			m.byAccountName[mbox.Account.Name] = make(map[string]Mailbox)
		}
		m.byAccountName[mbox.Account.Name][mbox.Name] = mbox
	}
	paths, err := gatherMboxPaths()
	if err != nil {
		return m, wrapMaybePermissionsError(err)
	}
	m.url2path = paths
	db, err := sql.Open("sqlite3", fmt.Sprintf("%s/Library/Mail/V10/MailData/Envelope Index", homeDir))
	if err != nil {
		return m, wrapMaybePermissionsError(err)
	}
	m.db = db
	return m, nil
}

func (m Mailboxes) Close() error {
	return m.db.Close()
}

// Message represents a Mail.app Email message.
type Message struct {
	pathWithoutExtension string
}

func (m Message) Open() (io.Reader, error) {
	f1, err := os.Open(path.Join(m.pathWithoutExtension + ".emlx"))
	if err == nil {
		return stripEmlx(f1)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	f2, err := os.Open(path.Join(m.pathWithoutExtension + ".partial.emlx"))
	if err == nil {
		return stripEmlx(f2)
	}
	return nil, err
}

// a ROWID of "0447630" becomes "4/4/0/0447630"
func emlPathFromROWID(ROWID string) string {
	return fmt.Sprintf("%s/%s/%s/Messages/%s", string(ROWID[2]), string(ROWID[1]), string(ROWID[0]), ROWID)
}

// stripEmlx takes an Apple-formatted ".emlx" file, and
// returns an io.Reader which strips the proprietary Apple
// parts of that file, so that other email parsers
// won't break.
func stripEmlx(r io.ReadSeeker) (io.Reader, error) {
	scanner := bufio.NewScanner(r)
	if scanner.Scan() {
		// The first line in eml specifies the number of bytes.
		original := scanner.Bytes()
		stringByteNum := bytes.Trim(original, " ")
		byteNum, err := strconv.Atoi(string(stringByteNum))
		if err != nil {
			return r, err
		}
		// use the length of the original line (which may
		// have some number of space characters) + the
		// line return character to reset the Seeker's
		// byte position
		r.Seek(int64(len(original)+1), io.SeekStart)
		return io.LimitReader(r, int64(byteNum)), nil
	}
	return r, errors.New("couldnt find the first line")
}

// Removes the home directory relative path
// and removes ".mbox" extensions from path elements
// to attempt to produce the same URL stored in the
// "Envelope Index" sqlite db.
func filePathToURL(rootPath, path string) string {
	sanitized := strings.Replace(path, rootPath, "", -1)
	sanitized = strings.Replace(sanitized, ".mbox", "", -1)
	return sanitized
}

// Returns a map of paths from the format stored in
// the "Envelope Index" database (minus the "imap://"
// protocol bit), to a path on disk relative to the main
// Mail library path.
func gatherMboxPaths() (map[string]string, error) {
	paths := map[string]string{}
	mboxPaths := map[string]bool{}
	walker := func(rootPath string) error {
		return filepath.WalkDir(rootPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "Data" || d.Name() == "MailData" {
					return filepath.SkipDir
				}
				for p := range mboxPaths {
					// We've marked this path as containing eml files,
					// via its parent directory
					if strings.HasPrefix(path, p) {
						// if this is a direct, non-.mbox child of
						// a marked parent directory, than this is the
						// actual directory containing eml files
						if strings.Replace(path, string(os.PathSeparator)+d.Name(), "", -1) == p {
							paths[filePathToURL(rootPath, p)] = path + string(os.PathSeparator) + "Data"
						}
					}
				}
				plistPath := filepath.Join(path, "Info.plist")
				_, err := os.Stat(plistPath)
				if err == nil {
					mboxPaths[path] = true
					return nil // allow it to descend a bit
				}
			}
			return nil
		})
	}
	err := walker(fmt.Sprintf("%s/Library/Mail/V10/", homeDir))
	if err != nil {
		return paths, err
	}
	return paths, nil
}
