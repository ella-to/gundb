package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	ucli "github.com/urfave/cli/v3"

	"ella.to/gundb/sea"
)

// Flags of put and set for SEA.

func signFlag() ucli.Flag {
	return &ucli.BoolFlag{
		Name:  "sign",
		Usage: "sign the value with your key (needs --username), so readers can --verify it",
	}
}
func encFlag() ucli.Flag {
	return &ucli.BoolFlag{
		Name:  "enc",
		Usage: "encrypt the value (needs --username): for yourself, or for --to",
	}
}
func toFlag() ucli.Flag {
	return &ucli.StringFlag{
		Name:  "to",
		Usage: "with --enc, encrypt for this `user` (alias or public key) instead of yourself; with --obf, give these users (comma-separated) access to the scope",
	}
}
func stringFlag() ucli.Flag {
	return &ucli.BoolFlag{
		Name:    "string",
		Aliases: []string{"s"},
		Usage:   "store the value as text, even if it looks like JSON (42, true, ...)",
	}
}

// Flags of get and sub for SEA.

func decFlag() ucli.Flag {
	return &ucli.BoolFlag{
		Name:  "dec",
		Usage: "decrypt encrypted values (needs --username); signed values are verified too",
	}
}
func verifyFlag() ucli.Flag {
	return &ucli.BoolFlag{
		Name:  "verify",
		Usage: "check signed values: fail unless --from (default: you) signed them, or they are in that user's own space",
	}
}
func fromFlag() ucli.Flag {
	return &ucli.StringFlag{
		Name:  "from",
		Usage: "with --dec or --verify, the `user` (alias or public key) who signed or encrypted the values",
	}
}

// parseValue turns the value argument into what to store: JSON if it
// parses as JSON, text otherwise. "-" reads stdin.
func parseValue(arg string, asString bool, stdin io.Reader) (any, error) {
	if arg == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("could not read the value from stdin: %w", err)
		}
		arg = strings.TrimSuffix(string(b), "\n")
	}
	if asString {
		return arg, nil
	}
	var v any
	d := json.NewDecoder(strings.NewReader(arg))
	d.UseNumber()
	if d.Decode(&v) != nil || d.More() {
		return arg, nil // not JSON: text
	}
	if _, isList := v.([]any); isList {
		return nil, fail(`GUN stores objects, not arrays: use an object like {"0":"a","1":"b"}, set, or --string`,
			"cannot store a JSON array")
	}
	return v, nil
}

// seal signs and/or encrypts v as the flags say. Encryption comes first, so
// a signed and encrypted value is signed ciphertext, like SEA does it.
func (cl *client) seal(ctx context.Context, c *ucli.Command, v any) (any, error) {
	sign, enc := c.Bool("sign"), c.Bool("enc")
	obf := cl.obf && cl.lastScope != nil
	if c.IsSet("to") && !enc && !obf {
		return nil, fail("add --enc to encrypt for that user", "--to only works with --enc")
	}
	if !sign && !enc {
		return v, nil
	}
	if cl.user == nil {
		return nil, fail("log in with --username and --password", "--sign and --enc use your keys, but you are not logged in")
	}
	pair := cl.user.Pair()
	switch {
	case enc && obf: // the scope's key: everyone with access reads it
		var err error
		if v, err = sea.Encrypt(v, cl.lastScope.key.ValueKey()); err != nil {
			return nil, fmt.Errorf("could not encrypt: %w", err)
		}
	case enc:
		to := userKeys{alias: "you", epub: pair.EPub}
		if who := c.String("to"); who != "" {
			var err error
			if to, err = cl.lookUp(ctx, who); err != nil {
				return nil, err
			}
		}
		secret, err := sea.Secret(to.epub, pair)
		if err != nil {
			return nil, fmt.Errorf("could not derive a key shared with %s: %w", to.alias, err)
		}
		if v, err = sea.Encrypt(v, secret); err != nil {
			return nil, fmt.Errorf("could not encrypt: %w", err)
		}
	}
	if sign {
		s, err := sea.Sign(v, pair)
		if err != nil {
			return nil, fmt.Errorf("could not sign: %w", err)
		}
		v = s
	}
	return v, nil
}

// opener verifies and decrypts SEA values on the way out.
type opener struct {
	verify bool
	strict bool   // --verify: an unsigned value fails, unless it is in the signer's own space
	inside bool   // the path is in the signer's own space, where only they can write
	pub    string // signer to verify against
	secret string // to decrypt with; "" = do not decrypt
	from   string // who pub belongs to, for messages

	sawSigned, sawEncrypted bool // values left as they are
}

// newOpener sets up --dec, --verify and --from, for reading path.
func (cl *client) newOpener(ctx context.Context, c *ucli.Command, path string) (*opener, error) {
	dec, verify := c.Bool("dec"), c.Bool("verify")
	o := &opener{verify: verify || dec, strict: verify}
	if c.IsSet("from") && !o.verify {
		return nil, fail("add --dec or --verify", "--from only works with --dec or --verify")
	}
	if !o.verify {
		return o, nil
	}
	if s := cl.lastScope; cl.obf && s != nil {
		// The owner is in the path; values are encrypted with the scope's key.
		if c.IsSet("from") {
			return nil, fail("drop --from: the scope's owner signed and encrypted it", "--from does not work with --obf")
		}
		o.pub, o.from, o.inside = s.owner.pub, strings.TrimPrefix(s.owner.alias, "~@"), true
		if dec {
			o.secret = s.key.ValueKey()
		}
		return o, nil
	}
	if dec && cl.user == nil {
		return nil, fail("log in with --username and --password", "--dec uses your keys, but you are not logged in")
	}
	var from userKeys
	switch who := c.String("from"); {
	case who != "":
		var err error
		if from, err = cl.lookUp(ctx, who); err != nil {
			return nil, err
		}
	case cl.user != nil:
		p := cl.user.Pair()
		from = userKeys{alias: cl.user.Alias(), pub: p.Pub, epub: p.EPub}
	default:
		return nil, fail("add --from <user>, or log in to check your own", "--verify needs to know whose signature to check")
	}
	o.pub, o.from = from.pub, from.alias
	if o.strict {
		space, err := cl.spaceOf(ctx, path)
		if err != nil {
			return nil, err
		}
		o.inside = space != "" && space == o.pub
	}
	if dec {
		secret, err := sea.Secret(from.epub, cl.user.Pair())
		if err != nil {
			return nil, fmt.Errorf("could not derive a key shared with %s: %w", from.alias, err)
		}
		o.secret = secret
	}
	return o, nil
}

// open returns raw (JSON) with every signed value verified and every
// encrypted one decrypted, as the opener was set up to.
func (o *opener) open(raw json.RawMessage, path string) (any, error) {
	if len(raw) == 0 {
		return nil, nil // deleted
	}
	v, err := decode(raw)
	if err != nil {
		return nil, err
	}
	return o.walk(v, path)
}

func (o *opener) walk(v any, path string) (any, error) {
	switch v := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if k == "_" {
				continue // get --links: the node's soul, not data
			}
			w, err := o.walk(v[k], path+"/"+k)
			if err != nil {
				return nil, err
			}
			v[k] = w
		}
		return v, nil
	case string:
		return o.leaf(v, path)
	}
	return v, o.unsigned(path) // a number, a bool or null: never signed
}

// unsigned is what --verify makes of a value that carries no signature:
// fine in the signer's own space, where only they can write (the server
// checks), and an error anywhere else, where anyone could have written it,
// replacing a signed value too.
func (o *opener) unsigned(path string) error {
	if !o.strict || o.inside {
		return nil
	}
	return fail("anyone can write here; only a value with a signature proves who wrote it",
		"%s is not signed, so it cannot be from %s", path, o.from)
}

// leaf opens one value. A decrypted or verified value can itself be
// sealed (signed ciphertext), so it goes round again.
func (o *opener) leaf(s string, path string) (any, error) {
	for range 3 {
		kind := seaKind(s)
		switch {
		case kind == "signed" && o.verify:
			raw, err := sea.Verify(s, o.pub)
			if err != nil {
				return nil, fail("pass the signer with --from <user>", "%s was not signed by %s", path, o.from)
			}
			if inner := "SEA" + string(raw); seaKind(inner) != "" {
				s = inner // SEA parses the message: sealed data comes back as an object
				continue
			}
			v, err := decode(raw)
			if err != nil {
				return nil, err
			}
			next, ok := v.(string)
			if !ok {
				return v, nil
			}
			s = next
		case kind == "encrypted" && o.secret != "":
			raw, err := sea.Decrypt(s, o.secret)
			if err != nil {
				return nil, fail("pass who encrypted it with --from <user>; it may be for someone else",
					"cannot decrypt %s with the key you share with %s", path, o.from)
			}
			v, err := decode(raw)
			if err != nil {
				return nil, err
			}
			next, ok := v.(string)
			if !ok {
				return v, nil
			}
			s = next
		default:
			if kind != "signed" {
				if err := o.unsigned(path); err != nil {
					return nil, err
				}
			}
			o.sawSigned = o.sawSigned || kind == "signed"
			o.sawEncrypted = o.sawEncrypted || kind == "encrypted"
			return s, nil
		}
	}
	return s, nil
}

// seaKind tells "signed" and "encrypted" SEA values from plain ones.
func seaKind(s string) string {
	body, ok := strings.CutPrefix(s, "SEA{")
	if !ok {
		return ""
	}
	var env map[string]json.RawMessage
	if json.Unmarshal([]byte("{"+body), &env) != nil {
		return ""
	}
	switch {
	case env["ct"] != nil && env["iv"] != nil:
		return "encrypted"
	case env["m"] != nil && env["s"] != nil:
		return "signed"
	}
	return ""
}

// note tells the person about sealed values they did not open.
func (o *opener) note(w io.Writer) {
	switch {
	case o.sawEncrypted:
		fmt.Fprintln(w, "note:  some values are encrypted; read them with --dec (and --from <user> if someone else wrote them)")
	case o.sawSigned:
		fmt.Fprintln(w, "note:  some values are signed; check them with --verify --from <user>")
	}
}

// decode parses JSON keeping numbers exact.
func decode(raw []byte) (any, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, fmt.Errorf("could not read the value: %w", err)
	}
	return v, nil
}

// encode writes v as JSON, indented or on one line.
func encode(v any, indent bool) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if indent {
		e.SetIndent("", "  ")
	}
	if err := e.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// short is v as one line of JSON, cut to fit a message.
func short(v any) string {
	s := encode(v, false)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}
