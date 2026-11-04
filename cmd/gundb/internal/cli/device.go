package cli

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	ucli "github.com/urfave/cli/v3"

	"ella.to/gundb/sea"
	"ella.to/gundb/sea/senderkey"
)

// A device is what this machine keeps for one user on one server, for
// group chats: the prekeys, the chains and the history. None of it goes
// into the graph: that is what lets a ratchet promise that a stolen
// password reads nothing, and a stolen device nothing it already read.
//
// It lives in one file, encrypted with a key derived from the password,
// readable by its owner only, and locked while a command uses it, so that
// group sub in one terminal and group send in another never undo each
// other's steps of a chain (a chain step reused would reuse a key).

func dirFlag() ucli.Flag {
	return &ucli.StringFlag{
		Name:    "dir",
		Usage:   "`directory` where this device keeps its group keys and history",
		Value:   defaultDir(),
		Sources: ucli.EnvVars("GUNDB_DIR"),
	}
}

func defaultDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ".gundb"
	}
	return filepath.Join(dir, "gundb")
}

// deviceState is the decrypted content of a device file.
type deviceState struct {
	Prekeys map[string]*sea.Pair   `json:"prekeys"` // by epub; the newest is published
	Groups  map[string]*groupState `json:"groups"`  // by "<owner pub>/<id>"
	Names   map[string]string      `json:"names"`   // alias by pub, for messages
}

// groupState is one group, as this device sees it.
type groupState struct {
	Label  string            `json:"label"` // alice/team, as given when first used
	Owner  string            `json:"owner"`
	ID     string            `json:"id"`
	Sender *senderkey.Sender `json:"sender,omitempty"` // my current chain
	// SharedWith maps each member my current chain is sealed for to the
	// prekey it was sealed to: a member who is no longer in the group
	// means a new chain, a new prekey means sealing it again.
	SharedWith map[string]string              `json:"shared_with"`
	Chains     map[string]*senderkey.Receiver `json:"chains"` // other members' chains, by ID
	Seen       map[string]bool                `json:"seen"`   // "<pub>/<message id>" handled
	Log        []logLine                      `json:"log"`    // what was said: a message decrypts once
}

type logLine struct {
	ID   string `json:"id"`
	From string `json:"from"` // pub
	At   int64  `json:"at"`   // unix ms, as the sender said
	Text string `json:"text"`
}

// deviceFile is the file on disk.
type deviceFile struct {
	V    int    `json:"v"`
	Pub  string `json:"pub"`
	Salt string `json:"salt"`
	Data string `json:"data"` // SEA ciphertext of deviceState
}

type device struct {
	path     string
	pub      string
	password string
	salt     string
	key      string // derived from password and salt, once
}

// deviceFor returns the device of the logged-in user of cl.
func deviceFor(c *ucli.Command, cl *client, password string) (*device, error) {
	dir := c.String("dir")
	if dir == "" {
		return nil, fail("pass --dir or set GUNDB_DIR", "no directory for the group keys")
	}
	u, err := url.Parse(cl.addr)
	if err != nil {
		return nil, err
	}
	host := strings.NewReplacer(":", "_", "/", "_").Replace(u.Host)
	return &device{
		path:     filepath.Join(dir, host, cl.user.Alias()+".json"),
		pub:      cl.user.Pub(),
		password: password,
	}, nil
}

// errNoDevice means group init has not been run on this device.
var errNoDevice = errors.New("no group keys on this device")

// update runs fn on the device's state, locked against other gundb
// commands, and saves what fn left unless it failed. With create, a
// missing file starts empty; otherwise it is errNoDevice.
func (d *device) update(create bool, fn func(*deviceState) error) error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(d.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return fmt.Errorf("could not lock %s: %w", lock.Name(), err)
	}
	defer unlockFile(lock)

	st, err := d.load()
	switch {
	case errors.Is(err, os.ErrNotExist) && create:
		st = &deviceState{}
		d.salt = rand.Text()
	case errors.Is(err, os.ErrNotExist):
		return errNoDevice
	case err != nil:
		return err
	}
	if st.Prekeys == nil {
		st.Prekeys = map[string]*sea.Pair{}
	}
	if st.Groups == nil {
		st.Groups = map[string]*groupState{}
	}
	if st.Names == nil {
		st.Names = map[string]string{}
	}
	if err := fn(st); err != nil {
		return err
	}
	return d.save(st)
}

func (d *device) load() (*deviceState, error) {
	b, err := os.ReadFile(d.path)
	if err != nil {
		return nil, err
	}
	var f deviceFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", d.path, err)
	}
	if f.Pub != d.pub {
		return nil, fmt.Errorf("%s belongs to another user (%s)", d.path, f.Pub)
	}
	if d.salt != f.Salt || d.key == "" {
		d.salt = f.Salt
		if d.key, err = sea.Work(d.password, d.salt); err != nil {
			return nil, err
		}
	}
	raw, err := sea.Decrypt(f.Data, d.key)
	if err != nil {
		return nil, fail("was the password changed? the file was written with the old one",
			"cannot open %s with this password", d.path)
	}
	var st deviceState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", d.path, err)
	}
	return &st, nil
}

// save writes st encrypted, through a temporary file, so a crash never
// leaves half a file.
func (d *device) save(st *deviceState) error {
	if d.key == "" {
		var err error
		if d.key, err = sea.Work(d.password, d.salt); err != nil {
			return err
		}
	}
	data, err := sea.Encrypt(st, d.key)
	if err != nil {
		return err
	}
	b, err := json.Marshal(deviceFile{V: 1, Pub: d.pub, Salt: d.salt, Data: data})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(d.path), ".device-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), d.path)
}
