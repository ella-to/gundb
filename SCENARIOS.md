# gundb scenarios

Hands-on scenarios for the `gundb` command line, from a first `put` to an
encrypted group chat. Each one is a series of commands you run in order,
with the output you should see and a few words on what just happened. Run
them top to bottom, or jump to the part you want to test.

The outputs were recorded on a real server. Random IDs, keys and times will
differ on yours; long keys are shortened with `…`. For how things work
underneath, see [GUIDE.md](GUIDE.md), section 14 for the CLI.

## Contents

- [Before you start](#before-you-start)
- **Part 1: Data**
  - [1. Write and read](#1-write-and-read)
  - [2. Objects merge, they don't replace](#2-objects-merge-they-dont-replace)
  - [3. Nested objects become linked nodes](#3-nested-objects-become-linked-nodes)
  - [4. Delete](#4-delete)
  - [5. Lists with set](#5-lists-with-set)
  - [6. Change and remove one item of a list](#6-change-and-remove-one-item-of-a-list)
  - [7. Link one node to another](#7-link-one-node-to-another)
  - [8. Lists of links: friends](#8-lists-of-links-friends)
  - [9. Watch changes live](#9-watch-changes-live)
- **Part 2: Users, signatures and encryption**
  - [10. Create users and log in](#10-create-users-and-log-in)
  - [11. Your own space](#11-your-own-space)
  - [12. Sign public data, and catch a forgery](#12-sign-public-data-and-catch-a-forgery)
  - [13. A private vault](#13-a-private-vault)
  - [14. Share a secret with one person](#14-share-a-secret-with-one-person)
  - [15. Private messages: encrypted and signed](#15-private-messages-encrypted-and-signed)
  - [16. Watch your encrypted notes live](#16-watch-your-encrypted-notes-live)
- **Part 3: Group chat**
  - [17. Set up each person's device](#17-set-up-each-persons-device)
  - [18. Create a group and invite two people](#18-create-a-group-and-invite-two-people)
  - [19. Talk](#19-talk)
  - [20. Live chat](#20-live-chat)
  - [21. Someone joins late](#21-someone-joins-late)
  - [22. Remove a member](#22-remove-a-member)
  - [23. A new device, without the keys](#23-a-new-device-without-the-keys)
- **Part 4: Running servers**
  - [24. Keep data on disk](#24-keep-data-on-disk)
  - [25. Two relays in sync](#25-two-relays-in-sync)
  - [26. A read replica](#26-a-read-replica)
  - [27. A write-only backup](#27-a-write-only-backup)
  - [28. Quotas](#28-quotas)
  - [29. See the graph](#29-see-the-graph)
- [When something goes wrong](#when-something-goes-wrong)
- [Start over](#start-over)

## Before you start

Build the command, or install it:

```sh
go install ./cmd/gundb          # from the repository; or: go build -o gundb ./cmd/gundb
```

Open **terminal 1** and start a server. It keeps everything in memory, so
stopping it (Ctrl-C) gives you a clean slate:

```sh
$ gundb server
07:29:11 INFO  storing data in memory: it is gone when the server stops (--store pebble keeps it)
07:29:11 INFO  gundb server is running  addr=http://localhost:8765/gun
```

Run everything else in **terminal 2**. Every command connects to
`http://localhost:8765/gun` by default.

Group chats keep their keys on your device. So that these scenarios don't
touch your real keys, point them at a scratch directory first:

```sh
export GUNDB_DIR=/tmp/gundb-scenarios
```

Two habits that save trouble:

- **Quote paths that start with `~`**: `'~/notes'`. Unquoted, your shell
  turns `~` into your home directory. gundb undoes that for `~/…`, but not
  for `~@alice/…`.
- **Quote values with spaces**: `'hello world'`. Otherwise the second word
  is an extra argument.

Optionally, open **terminal 3** with `gundb visual` (see
[29](#29-see-the-graph)) and watch the graph change as you go.

---

# Part 1: Data

## 1. Write and read

A path names a place in the graph, one key per segment: `users/1/name` is
the field `name` of the node `users/1`. A value is JSON if it parses as
JSON, text otherwise.

```sh
$ gundb put users/1/name Ali
ok  users/1/name = "Ali"

$ gundb put users/1/score 42              # a number
ok  users/1/score = 42

$ gundb put users/1/code 42 --string      # the text "42"
ok  users/1/code = "42"

$ gundb put users/1/zip 01234             # not valid JSON, so text
ok  users/1/zip = "01234"

$ gundb get users/1
{
  "code": "42",
  "name": "Ali",
  "score": 42,
  "zip": "01234"
}

$ gundb get users/1/name
"Ali"
```

A value can come from stdin with `-`, handy for files and other programs:

```sh
$ echo '{"bio":"from stdin"}' | gundb put users/1/about -
ok  users/1/about = {"bio":"from stdin"}
```

Top-level nodes hold fields, not single values. That's GUN's model, and
gundb tells you so:

```sh
$ gundb put greeting 'hello world'
error: greeting is a top-level node: it holds fields, not a single value
hint:  put a field: gundb put greeting/name hello world, or an object: gundb put greeting '{"name":...}'
```

**Check:** `get` on the node shows every field; numbers stay numbers unless
you pass `--string`.

## 2. Objects merge, they don't replace

Putting an object writes its fields; fields you leave out stay as they
were. Two people can update different fields of the same node without
overwriting each other.

```sh
$ gundb put users/2 '{"name":"Bob","age":25}'
ok  users/2 = {"age":25,"name":"Bob"}

$ gundb put users/2 '{"city":"Oslo"}'
ok  users/2 = {"city":"Oslo"}

$ gundb get users/2
{
  "age": 25,
  "city": "Oslo",
  "name": "Bob"
}
```

**Check:** `name` and `age` survived the second put. To remove a field,
delete it ([4](#4-delete)).

## 3. Nested objects become linked nodes

GUN has no nested objects: each inner object becomes a node of its own,
linked from its parent. `get` follows the links for you; `get --links`
shows the graph as stored.

```sh
$ gundb put users/3 '{"name":"Cara","address":{"city":"Berlin","zip":"10115"}}'
ok  users/3 = {"address":{"city":"Berlin","zip":"10115"},"name":"Cara"}

$ gundb get users/3
{
  "address": {
    "city": "Berlin",
    "zip": "10115"
  },
  "name": "Cara"
}

$ gundb get users/3 --links
{
  "_": {
    "#": "users/3"
  },
  "address": {
    "#": "users/3/address"
  },
  "name": "Cara"
}
```

`"_": {"#": …}` is the node's soul, its ID; `{"#": "users/3/address"}` is a
link. Paths walk through links, so you can read and write deep fields
directly:

```sh
$ gundb put users/3/address/city Hamburg
ok  users/3/address/city = "Hamburg"

$ gundb get users/3/address/city
"Hamburg"
```

## 4. Delete

Deleting is writing `null`. GUN never really forgets a field (it keeps
the `null`, so the deletion syncs to every peer), but it disappears from
reads.

```sh
$ gundb put users/2/city null
ok  users/2/city = null

$ gundb get users/2
{
  "age": 25,
  "name": "Bob"
}

$ gundb put users/2/name null
ok  users/2/name = null

$ gundb put users/2/age null
ok  users/2/age = null

$ gundb get users/2
{}

$ gundb get users/9
error: nothing at users/9
hint:  write something there first with: gundb put <path> <value>
```

**Check:** a node with every field deleted reads as `{}`; one that never
existed is "nothing at".

## 5. Lists with set

`set` adds an item to an unordered list under a new random key, like
`gun.set` in JS, and prints the new item's path. An object becomes a node
of its own; a plain value is stored in the list node.

```sh
$ gundb set todos '{"title":"buy milk","done":false}'
ok  todos/mw4y66pc000kaqyUEHpz = {"done":false,"title":"buy milk"}

$ gundb set todos '{"title":"call mom","done":false}'
ok  todos/mw4y66q9000tD5WkIhFx = {"done":false,"title":"call mom"}

$ gundb set todos '{"title":"write report","done":false}'
ok  todos/mw4y66r5000E3fCDQN2N = {"done":false,"title":"write report"}

$ gundb get todos
{
  "mw4y66pc000kaqyUEHpz": {
    "done": false,
    "title": "buy milk"
  },
  "mw4y66q9000tD5WkIhFx": {
    "done": false,
    "title": "call mom"
  },
  "mw4y66r5000E3fCDQN2N": {
    "done": false,
    "title": "write report"
  }
}
```

A list of plain values:

```sh
$ gundb set users/1/tags go
ok  users/1/tags/mw4y66tw000BKeMtDCM7 = "go"

$ gundb set users/1/tags gun
ok  users/1/tags/mw4y66ur0009km57nR9e = "gun"

$ gundb get users/1/tags
{
  "mw4y66tw000BKeMtDCM7": "go",
  "mw4y66ur0009km57nR9e": "gun"
}
```

**Check:** adding the same title twice gives two items: a set has no
order and no uniqueness. For a list with stable keys (one entry per user,
say), use `put` with your own key: `gundb put todos/milk '{…}'`.

## 6. Change and remove one item of a list

`set` prints the item's path on stdout, so a script can keep it:

```sh
$ MILK=$(gundb set todos '{"title":"buy milk","done":false}' | awk '{print $2}')
$ echo $MILK
todos/mw4y6ijn000XUh63LGxu

$ gundb put $MILK/done true
ok  todos/mw4y6ijn000XUh63LGxu/done = true

$ gundb get $MILK
{
  "done": true,
  "title": "buy milk"
}
```

Removing an item from the list is deleting its key:

```sh
$ gundb put $MILK null
ok  todos/mw4y6ijn000XUh63LGxu = null

$ gundb get todos          # the item is gone from the list
{
  "mw4y6iip000MDCLP26dk": {
    "done": false,
    "title": "buy milk"
  },
  "mw4y6iki000gfZfvjR1E": {
    "done": false,
    "title": "call mom"
  }
}
```

**Check:** that removed the link from the list. The item's node itself is
still there, unreachable from `todos`; delete its fields too if it holds
something you want gone.

## 7. Link one node to another

`--link <path>` stores a link instead of a value. Readers then see the
linked node, always current, not a copy.

```sh
$ gundb put people/alice '{"name":"Alice"}'
$ gundb put people/bob '{"name":"Bob"}'
$ gundb put people/carol '{"name":"Carol"}'

$ gundb put people/alice/best --link people/bob
ok  people/alice/best links to people/bob

$ gundb get people/alice
{
  "best": {
    "name": "Bob"
  },
  "name": "Alice"
}

$ gundb put people/bob/name Robert        # change bob...
ok  people/bob/name = "Robert"

$ gundb get people/alice/best/name        # ...alice's link sees it
"Robert"
```

gundb checks the target exists, so a typo doesn't leave a dangling link:

```sh
$ gundb put people/alice/best --link people/nobody
error: nothing at people/nobody to link to
hint:  write it first, e.g. gundb put people/nobody '{"name":...}', or check the path with gundb get

$ gundb put people/alice/best null        # remove the link; bob stays
ok  people/alice/best = null
```

## 8. Lists of links: friends

`set --link` adds a link to a list; `unset --link` removes it. Neither
touches the linked node.

```sh
$ gundb set people/alice/friends --link people/bob
ok  people/alice/friends now has a link to people/bob

$ gundb set people/alice/friends --link people/carol
ok  people/alice/friends now has a link to people/carol

$ gundb get people/alice/friends
{
  "people/bob": {
    "name": "Robert"
  },
  "people/carol": {
    "name": "Carol"
  }
}

$ gundb get people/alice/friends --links
{
  "_": {
    "#": "people/alice/friends"
  },
  "people/bob": {
    "#": "people/bob"
  },
  "people/carol": {
    "#": "people/carol"
  }
}

$ gundb unset people/alice/friends --link people/carol
ok  people/alice/friends no longer links to people/carol

$ gundb get people/carol                  # carol is still there
{
  "name": "Carol"
}

$ gundb unset people/alice/friends --link people/carol
error: people/alice/friends does not link to people/carol
hint:  see its links with gundb get people/alice/friends --links
```

**Check:** a set of links is keyed by the linked soul, so adding the same
friend twice keeps one entry.

## 9. Watch changes live

`sub` prints a path's value now and after every change, until Ctrl-C.
Start it in **terminal 3** (or a third tab), then write from terminal 2.

```sh
# terminal 3
$ gundb sub rooms/lobby
07:25:29 INFO  watching rooms/lobby (Ctrl-C to stop)  addr=http://localhost:8765/gun
```

```sh
# terminal 2
$ gundb put rooms/lobby/topic 'welcome'
$ gundb put rooms/lobby/online 3
$ gundb put rooms/lobby/topic 'standup at 10'
$ gundb put rooms/lobby/online null
```

Terminal 3 prints each new state:

```
07:25:30  {"topic":"welcome"}
07:25:30  {"online":3,"topic":"welcome"}
07:25:30  {"online":3,"topic":"standup at 10"}
07:25:31  {"topic":"standup at 10"}
```

For a list, `--map` prints each item as it changes, with its key, instead
of the whole list. A tiny chat room:

```sh
# terminal 3
$ gundb sub chat --map
```

```sh
# terminal 2
$ gundb set chat '{"from":"alice","text":"hi"}'
$ gundb set chat '{"from":"bob","text":"hey!"}'
```

```
07:25:31  mw4y6qsz000J2lbsuxfw  {"from":"alice","text":"hi"}
07:25:31  mw4y6r2o000RTs2eBBJm  {"from":"bob","text":"hey!"}
```

**Check:** every write shows up in terminal 3 within a moment. Run a second
`sub` in another window: both see every change.

---

# Part 2: Users, signatures and encryption

## 10. Create users and log in

A user is a SEA key pair. The server stores it the way `gun.user()` does:
the private keys encrypted with the password, so the same user can log in
from GUN.js in a browser. Create three:

```sh
$ gundb user create -u alice -p alice-secret
created user alice
  pub   gAXUXPaCr6R8bQE3wb1n…
  epub  9qXgjsgfZcJp4kmUaO_d…
next:  gundb put ~/profile/name alice -u alice -p <password>

$ gundb user create -u bob -p bob-secret
$ gundb user create -u carol -p carol-secret
```

`pub` signs and verifies; `epub` is for encryption. Logging in checks the
password and shows them again:

```sh
$ gundb user login -u alice -p alice-secret
logged in as alice
  pub   gAXUXPaCr6R8bQE3wb1n…
  epub  9qXgjsgfZcJp4kmUaO_d…

$ gundb user login -u alice -p wrong
error: wrong username or password
hint:  check --username and --password; create a user with: gundb user create

$ gundb user create -u alice -p short
error: could not create user alice: password too short (8 characters minimum)
```

Tired of typing `-u` and `-p`? Set them once for the shell (the scenarios
below keep them written out, since they switch users a lot):

```sh
export GUNDB_USERNAME=alice
read -s GUNDB_PASSWORD && export GUNDB_PASSWORD   # type it; nothing is shown
```

## 11. Your own space

Logged in, `~/` is your own space, `~<pub>/…`. Every value you write there
is signed with your key, and every server and peer checks the signature,
so only you can write it. Anyone can read it, as `~@alias/…`.

```sh
$ gundb put '~/profile' '{"name":"Alice","city":"Oslo"}' -u alice -p alice-secret
ok  ~/profile = {"city":"Oslo","name":"Alice"}

$ gundb get '~@alice/profile'               # anyone, no login
{
  "city": "Oslo",
  "name": "Alice"
}
```

Bob tries to change alice's name:

```sh
$ gundb put '~@alice/profile/name' 'Mallory' -u bob -p bob-secret
error: the server rejected the write: SEA: unverified data: ~gAXUXPaCr6R8…profile: unsigned data
hint:  a user's space can only be written by that user: log in with --username and write to ~/...

$ gundb get '~@alice/profile/name'
"Alice"
```

**Check:** the server refused bob's write; alice's profile is unchanged.

## 12. Sign public data, and catch a forgery

Outside a user's space anyone can write anything. `--sign` attaches your
signature to a value, so readers can check who wrote it with
`--verify --from <user>`.

```sh
$ gundb put news/today 'Release 1.0 is out' --sign -u alice -p alice-secret
ok  news/today = "Release 1.0 is out" (signed)

$ gundb get news/today                      # as stored
"SEA{\"m\":\"Release 1.0 is out\",\"s\":\"Ed71xw1BTkWWqfb/qqUk…\"}"
note:  some values are signed; check them with --verify --from <user>

$ gundb get news/today --verify --from alice
"Release 1.0 is out"

$ gundb get news/today --verify --from bob
error: news/today was not signed by bob
hint:  pass the signer with --from <user>
```

Now someone replaces the announcement, without alice's key:

```sh
$ gundb put news/today 'Release 2.0 is out'
ok  news/today = "Release 2.0 is out"

$ gundb get news/today --verify --from alice
error: news/today is not signed, so it cannot be from alice
hint:  anyone can write here; only a value with a signature proves who wrote it
```

Signing doesn't stop the overwrite (nothing outside a user's space can),
but it makes it visible. For data only alice may change, write it in her
space instead ([11](#11-your-own-space)). There, `--verify` passes plain
values too, since only alice can write them:

```sh
$ gundb get '~@alice/profile' --verify --from alice
{
  "city": "Oslo",
  "name": "Alice"
}
```

## 13. A private vault

`--enc` encrypts a value with your own keys. The server, and anyone
reading it, sees only ciphertext; `--dec` decrypts it for you.

```sh
$ gundb put '~/vault/bank' 'IBAN DE89 3704 0044' --enc -u alice -p alice-secret
ok  ~/vault/bank = (encrypted)

$ gundb put '~/vault/pin' 4711 --enc -u alice -p alice-secret
ok  ~/vault/pin = (encrypted)

$ gundb get '~/vault' -u alice -p alice-secret
{
  "bank": "SEA{\"ct\":\"xIyU2DmjPyuJtYTMibsmXYPj…\",\"iv\":\"RG+BkUYAUQwICcWimK+1\",\"s\":\"VlEMBTd1vWu9\"}",
  "pin": "SEA{\"ct\":\"tnKPdhmwH8CRyxZ3NV1n80lQzuU=\",\"iv\":\"OR++7Qt5d+1Px6+vTAyr\",\"s\":\"yEhBqPbOJtCl\"}"
}
note:  some values are encrypted; read them with --dec (and --from <user> if someone else wrote them)

$ gundb get '~/vault' --dec -u alice -p alice-secret
{
  "bank": "IBAN DE89 3704 0044",
  "pin": 4711
}

$ gundb get '~@alice/vault/bank' --dec --from alice -u bob -p bob-secret
error: cannot decrypt ~@alice/vault/bank with the key you share with alice
hint:  pass who encrypted it with --from <user>; it may be for someone else
```

**Check:** the number `4711` comes back as a number: the type survives
encryption. Keep in mind that the *paths* are not secret. The server sees
`vault/bank`. Use dull key names (`~/vault/1`) when the name says too much.

## 14. Share a secret with one person

`--enc --to bob` encrypts with a key that only alice and bob can work out
(SEA's ECDH: alice's private key with bob's public one, or the other way
round). Bob reads it with `--dec --from alice`.

```sh
$ gundb put '~/shared/bob/wifi' 'hunter2' --enc --to bob -u alice -p alice-secret
ok  ~/shared/bob/wifi = (encrypted)

$ gundb get '~@alice/shared/bob/wifi' --dec --from alice -u bob -p bob-secret
"hunter2"

$ gundb get '~@alice/shared/bob/wifi' --dec --from alice -u carol -p carol-secret
error: cannot decrypt ~@alice/shared/bob/wifi with the key you share with alice
hint:  pass who encrypted it with --from <user>; it may be for someone else
```

Since it sits in alice's space, bob also knows alice wrote it. To take it
back, delete it (bob may have kept a copy, as with any secret you share):

```sh
$ gundb put '~/shared/bob/wifi' null -u alice -p alice-secret
ok  ~/shared/bob/wifi = null

$ gundb get '~@alice/shared/bob/wifi' --dec --from alice -u bob -p bob-secret
error: nothing at ~@alice/shared/bob/wifi
```

## 15. Private messages: encrypted and signed

An inbox outside anyone's space, like `inbox/bob`, can be written by
anyone, which is what an inbox needs. Encrypt messages for the reader, and
sign them, so the reader knows who sent each one.

Alice writes to bob:

```sh
$ gundb put inbox/bob/1 'meet at 6, bring the keys' --enc --to bob --sign -u alice -p alice-secret
ok  inbox/bob/1 = (encrypted, signed)

$ gundb get inbox/bob/1                     # what everyone else sees
"SEA{\"m\":{\"ct\":\"8GC125a1Sl53cBcbyBSIS8CK…\",\"iv\":\"9cbJmjhChWi42fllXunE\",\"s\":\"jmzt1sb9FwtG\"},\"s\":\"ANUKqUaccbT77ZNt…\"}"
note:  some values are signed; check them with --verify --from <user>

$ gundb get inbox/bob/1 --dec --from alice -u bob -p bob-secret
"meet at 6, bring the keys"

$ gundb get inbox/bob/1 --dec --from carol -u bob -p bob-secret
error: inbox/bob/1 was not signed by carol
hint:  pass the signer with --from <user>
```

Bob answers in alice's inbox:

```sh
$ gundb put inbox/alice/1 'ok, see you' --enc --to alice --sign -u bob -p bob-secret
ok  inbox/alice/1 = (encrypted, signed)

$ gundb get inbox/alice/1 --dec --from bob -u alice -p alice-secret
"ok, see you"
```

Carol tries to pass a message off as alice's. Without `--sign` the sender
is still pinned down: the key it's encrypted with is one only carol and
bob can derive, so it opens with `--from carol`, never with
`--from alice`:

```sh
$ gundb put inbox/bob/2 'from alice, honestly' --enc --to bob -u carol -p carol-secret
ok  inbox/bob/2 = (encrypted)

$ gundb get inbox/bob/2 --dec --from alice -u bob -p bob-secret
error: cannot decrypt inbox/bob/2 with the key you share with alice
hint:  pass who encrypted it with --from <user>; it may be for someone else

$ gundb get inbox/bob/2 --dec --from carol -u bob -p bob-secret
"from alice, honestly"
```

**Discussion:** the shared key proves the sender to bob alone. A signature
proves it to anyone, which matters if bob ever needs to show a third
person what alice wrote. And since anyone can overwrite `inbox/bob/1`,
the signature is also how bob notices a message was swapped.

## 16. Watch your encrypted notes live

`sub` takes `--dec` too. Leave this running in terminal 3:

```sh
$ gundb sub '~/notes' --map --dec -u alice -p alice-secret
07:29:53 INFO  watching ~/notes (Ctrl-C to stop)  addr=http://localhost:8765/gun
```

```sh
# terminal 2
$ gundb put '~/notes/1' 'buy a gift for bob' --enc -u alice -p alice-secret
$ gundb put '~/notes/2' 'dentist on friday' --enc -u alice -p alice-secret
$ gundb put '~/notes/1' null -u alice -p alice-secret
```

```
07:29:54  1  "buy a gift for bob"
07:29:55  2  "dentist on friday"
07:29:55  1  null
```

**Check:** the server only ever saw ciphertext; terminal 3 shows plain text
because it holds alice's keys.

---

# Part 3: Group chat

`gundb group` is an end-to-end encrypted group chat with sender keys, the
ratchet Signal and WhatsApp use for groups. Every message has its own key,
used once and thrown away. The keys live on each person's device (in
`GUNDB_DIR`), never on the server.

In real life each person runs these commands on their own machine. Here
all three share one machine, which works: the device directory keeps one
file per user and server.

## 17. Set up each person's device

Each person runs `init` once per device. Until they do, they can't create
groups or be added to one:

```sh
$ gundb group create team -u alice -p alice-secret
error: this device has no group keys for alice
hint:  run once on this device: gundb group init -u alice -p <password> (or point --dir at your keys)

$ gundb group init -u alice -p alice-secret
ok  alice can now create and join groups (keys in /tmp/gundb-scenarios/localhost_8765/alice.json)

$ gundb group init -u bob -p bob-secret
ok  bob can now create and join groups (keys in /tmp/gundb-scenarios/localhost_8765/bob.json)

$ gundb group init -u carol -p carol-secret
ok  carol can now create and join groups (keys in /tmp/gundb-scenarios/localhost_8765/carol.json)
```

## 18. Create a group and invite two people

Alice creates `team` and adds bob and carol. She calls it `team`; everyone
else calls it `alice/team`.

```sh
$ gundb group create team -u alice -p alice-secret
ok  created team; others name it alice/team

$ gundb group add team bob carol -u alice -p alice-secret
ok  added bob, carol to team; they read it as alice/team

$ gundb group members alice/team -u bob -p bob-secret
alice  (owner)
bob
carol
```

## 19. Talk

```sh
$ gundb group send team 'welcome to the team chat!' -u alice -p alice-secret
ok  sent to alice/team

$ gundb group read alice/team -u bob -p bob-secret
2026-11-06 07:27  alice: welcome to the team chat!

$ gundb group send alice/team 'thanks! glad to be here' -u bob -p bob-secret
$ gundb group send alice/team 'standup at 10 tomorrow?' -u bob -p bob-secret
$ gundb group send team 'yes, 10 works' -u alice -p alice-secret

$ gundb group read alice/team -u carol -p carol-secret
2026-11-06 07:27  alice: welcome to the team chat!
2026-11-06 07:27  bob: thanks! glad to be here
2026-11-06 07:27  bob: standup at 10 tomorrow?
2026-11-06 07:27  alice: yes, 10 works

$ gundb group send alice/team 'see you all there' -u carol -p carol-secret
```

`--new` prints only what arrived since your last read:

```sh
$ gundb group read alice/team --new -u bob -p bob-secret
2026-11-06 07:27  alice: yes, 10 works
2026-11-06 07:27  carol: see you all there
```

`-` sends stdin, for longer messages:

```sh
$ printf 'line one\nline two\n' | gundb group send team - -u alice -p alice-secret
ok  sent to alice/team

$ gundb group ls -u alice -p alice-secret
alice/team  (6 messages)
```

**Discussion:** each message can be decrypted only once, because its key
is deleted after use. `read` still shows the whole conversation because
each device keeps what it already read, in its device file. Run `read`
twice and compare: same history, no network secrets reused.

## 20. Live chat

`group sub` prints the conversation so far, then each message as it
arrives. Carol keeps it open in terminal 3:

```sh
$ gundb group sub alice/team -u carol -p carol-secret
2026-11-06 07:27  alice: welcome to the team chat!
…
07:27:27 INFO  watching alice/team (Ctrl-C to stop)  addr=http://localhost:8765/gun
```

```sh
# terminal 2
$ gundb group send alice/team 'is the build green?' -u bob -p bob-secret
$ gundb group send team 'yes, shipping now' -u alice -p alice-secret
```

```
2026-11-06 07:27  bob: is the build green?
2026-11-06 07:27  alice: yes, shipping now
```

## 21. Someone joins late

Dave joins after the conversation started. He reads from the moment he
joined, never the history.

```sh
$ gundb user create -u dave -p dave-secret

$ gundb group add team dave -u alice -p alice-secret
error: dave has not set up group chats yet
hint:  ask them to run once: gundb group init -u dave -p <password>

$ gundb group init -u dave -p dave-secret
$ gundb group add team dave -u alice -p alice-secret
ok  added dave to team; they read it as alice/team

$ gundb group send team 'welcome dave!' -u alice -p alice-secret

$ gundb group read alice/team -u dave -p dave-secret
2026-11-06 07:27  alice: welcome dave!
```

Members share their keys with a newcomer the next time they use the group.
Until bob does, dave's read may note that bob's messages wait for a key.
Once bob writes:

```sh
$ gundb group send alice/team 'hi dave' -u bob -p bob-secret

$ gundb group read alice/team -u dave -p dave-secret
2026-11-06 07:27  alice: welcome dave!
2026-11-06 07:27  bob: hi dave
```

**Check:** nothing from before `welcome dave!` ever shows up for dave.

## 22. Remove a member

Alice removes carol. Everyone's next message goes out on a new chain whose
key carol never gets.

```sh
$ gundb group remove team carol -u alice -p alice-secret
ok  removed carol from team; your new chain is not shared with them

$ gundb group members team -u alice -p alice-secret
alice  (owner)
bob
dave

$ gundb group send team 'carol cannot read this' -u alice -p alice-secret

$ gundb group read alice/team --new -u bob -p bob-secret
2026-11-06 07:27  alice: carol cannot read this

$ gundb group read alice/team -u carol -p carol-secret
error: you are not a member of alice/team
hint:  ask the owner to add you: gundb group add team carol

$ gundb group send alice/team 'can I still post?' -u carol -p carol-secret
error: you are not a member of alice/team
```

Only the owner manages members:

```sh
$ gundb group add team bob -u bob -p bob-secret
error: you can't add yourself
hint:  you own it
```

## 23. A new device, without the keys

Group keys never leave the device. Bob's password alone, on a new laptop
(a fresh `--dir`), opens nothing:

```sh
$ gundb group read alice/team --dir /tmp/new-laptop -u bob -p bob-secret
error: this device has no group keys for bob
hint:  run once on this device: gundb group init -u bob -p <password> (or point --dir at your keys)
```

**Discussion:** that is the point (a stolen password reads no group chat)
and the cost: lose the device file and you lose the history. Back up
`GUNDB_DIR`. To continue on the new laptop, bob runs `group init` there;
the others share their chains with it the next time they use the group,
and he reads from then on.

---

# Part 4: Running servers

Stop the server in terminal 1 (Ctrl-C) before these; each starts its own.

## 24. Keep data on disk

`--store pebble` keeps everything on disk, every version included.

```sh
# terminal 1
$ gundb server --store pebble --data ./data
07:29:05 INFO  storing data on disk with pebble  data=/home/you/data
07:29:05 INFO  gundb server is running  addr=http://localhost:8765/gun
```

```sh
# terminal 2
$ gundb put notes/1 '{"text":"survives restarts"}'
ok  notes/1 = {"text":"survives restarts"}
```

Stop the server (Ctrl-C), start it again with the same command, then:

```sh
$ gundb get notes/1
{
  "text": "survives restarts"
}
```

One server per data directory:

```sh
$ gundb server --addr http://localhost:8766/gun --data ./data
error: cannot open the data directory ./data: pebblestore: resource temporarily unavailable
hint:  is another gundb server using it? each server needs its own --data
```

## 25. Two relays in sync

Relays sync with `--peers`. Start two:

```sh
# terminal 1
$ gundb server

# terminal 3
$ gundb server --addr http://localhost:8766/gun --peers http://localhost:8765/gun
07:29:08 INFO  gundb server is running  addr=http://localhost:8766/gun
07:29:08 INFO  syncing with peer  peer=http://localhost:8765/gun
07:29:08 INFO  connected to peer  peer=http://localhost:8765/gun
```

Write to one, read from the other, both ways:

```sh
$ gundb put city/oslo/temp 4                               # to 8765
ok  city/oslo/temp = 4

$ gundb get city/oslo -a http://localhost:8766/gun         # from 8766
{
  "temp": 4
}

$ gundb put city/oslo/temp 5 -a http://localhost:8766/gun  # to 8766
$ gundb get city/oslo/temp                                 # from 8765
5
```

**Check:** stop the first relay, write `city/oslo/temp 9` to the second,
and start the first again. The second reconnects within 2s. Reading from
the first then returns `9`: a relay asks its peers for what it doesn't
have, and keeps what they send, so it still has `9` after the second
stops. (What it never reads isn't copied over on reconnect; run every
relay with `--store pebble` and the same `--peers` to keep them all
complete.)

## 26. A read replica

`?gundb_mode=read` on a URL in `--peers` follows that server without ever
writing to it.
Think of a relay near your users that serves reads from a primary, or a
staging server on production data. Stop the servers from 25, then:

```sh
# terminal 1: the primary
$ gundb server

# terminal 3: the replica
$ gundb server --addr http://localhost:8766/gun --peers 'http://localhost:8765/gun?gundb_mode=read'
08:59:24 INFO  gundb server is running  addr=http://localhost:8766/gun
08:59:24 INFO  reading from peer, never writing to it  peer=http://localhost:8765/gun
08:59:24 INFO  connected to peer  peer=http://localhost:8765/gun
```

The replica reads the primary's data, and follows its changes live:

```sh
$ gundb put prices/milk 2                                  # on the primary
ok  prices/milk = 2

$ gundb get prices -a http://localhost:8766/gun            # on the replica
{
  "milk": 2
}

$ gundb sub prices/milk -a http://localhost:8766/gun       # terminal 4, on the replica
08:59:26  2
```

```sh
$ gundb put prices/milk 3                                  # on the primary
```

```
08:59:27  3
```

Now write to the replica:

```sh
$ gundb put prices/milk 0 -a http://localhost:8766/gun
ok  prices/milk = 0

$ gundb get prices/milk                                    # the primary: unchanged
3

$ gundb get prices/milk -a http://localhost:8766/gun       # the replica: its own value
0
```

**Discussion:** the write never reached the primary, which is the point.
But the replica stored it and serves it to its own clients until the
primary changes `milk` again. For a strictly read-only replica, refuse
client writes on it too: in Go, `CanWrite` returning `false` (see
GUIDE.md, "One-way peers"). The primary, for its part, gets nothing from
the replica: its gets to the replica are answered "not found".

## 27. A write-only backup

`?gundb_mode=write` sends every write to a server and never takes anything
from it. Here, a backup that keeps everything on disk. Even a restored
or tampered backup can't push data back into the live server.

```sh
# terminal 1: the backup
$ gundb server --addr http://localhost:8766/gun --store pebble --data ./backup

# terminal 3: the live server
$ gundb server --peers 'http://localhost:8766/gun?gundb_mode=write'
08:59:25 INFO  gundb server is running  addr=http://localhost:8765/gun
08:59:25 INFO  writing to peer, never reading from it  peer=http://localhost:8766/gun
08:59:25 INFO  connected to peer  peer=http://localhost:8766/gun
```

Every write to the live server lands on the backup:

```sh
$ gundb put orders/1 '{"item":"milk","qty":2}'
ok  orders/1 = {"item":"milk","qty":2}

$ gundb get orders/1 -a http://localhost:8766/gun          # on the backup
{
  "item": "milk",
  "qty": 2
}
```

What the backup holds or writes never comes back:

```sh
$ gundb put orders/1/qty 999 -a http://localhost:8766/gun  # someone changes the backup
ok  orders/1/qty = 999

$ gundb get orders/1/qty                                   # live: unchanged
2

$ gundb put notes/x 'only on the backup' -a http://localhost:8766/gun
$ gundb get notes/x                                        # live never asks the backup
error: nothing at notes/x
```

Quote URLs with `?` or `&`. Unquoted, zsh treats `?` as a wildcard and
fails before gundb even runs (bash passes it through, but `&` would end
the command):

```sh
$ gundb server --peers http://localhost:8766/gun?gundb_mode=write
zsh: no matches found: http://localhost:8766/gun?gundb_mode=write
```

A typo in the mode is caught before anything is dialled, instead of
quietly syncing both ways:

```sh
$ gundb server --peers 'http://localhost:8766/gun?gundb_mode=reed'
error: bad --peers: peer "http://localhost:8766/gun?gundb_mode=reed": gundb_mode=reed is not read, write or both
hint:  add ?gundb_mode=read to only read from a peer, ?gundb_mode=write to only write to it, or leave it out for both ways
```

## 28. Quotas

`--max-nodes` and `--max-bytes` give every SEA user the same quota for
their space. A tiny one, to hit it quickly:

```sh
# terminal 1
$ gundb server --max-nodes 3 --max-bytes 10KB
07:29:27 INFO  each user may keep  nodes=3  data=10KB
07:29:27 INFO  gundb server is running  addr=http://localhost:8765/gun
```

```sh
$ export GUNDB_USERNAME=erin GUNDB_PASSWORD=erin-secret
$ gundb user create                      # her account: node 1
$ gundb put '~/notes/1' 'first'          # ~/notes: node 2
ok  ~/notes/1 = "first"

$ gundb put '~/todo/1' 'milk'            # ~/todo: node 3
ok  ~/todo/1 = "milk"

$ gundb put '~/more/1' 'one too many'    # node 4
error: the server refused to write ~/more/1: quota exceeded: fAGfhT_CnmTV… may keep 3 nodes, this write would make it 4
hint:  free some space by deleting what you no longer need (gundb put <path> null), or move to a bigger plan
```

Deleting the only field of `~/todo` frees that node:

```sh
$ gundb put '~/todo/1' null
ok  ~/todo/1 = null

$ gundb put '~/more/1' 'fits now'
ok  ~/more/1 = "fits now"
```

Bytes count too:

```sh
$ head -c 12000 /dev/zero | tr '\0' x | gundb put '~/notes/1' -
error: the server refused to write ~/notes/1: quota exceeded: … may keep 10240 bytes, this write would make it 13135

$ head -c 4000 /dev/zero | tr '\0' x | gundb put '~/notes/1' -
ok  ~/notes/1 = "xxxxxxxxxxxxxxxxxxxxxxxx…
```

Data outside users' spaces isn't anyone's, so it isn't limited:

```sh
$ gundb put public/x 1
ok  public/x = 1
```

**Discussion:** a new account already takes about 900 bytes (its keys,
stored encrypted), and every value in a user's space carries a signature
of about 100 bytes. With `--max-bytes 1KB` even the first note is refused.
Plans per user, and what to check before you bill for storage, are in
GUIDE.md: "Quotas: plans and usage" and "Billing with quotas".

Unset the variables when you're done: `unset GUNDB_USERNAME GUNDB_PASSWORD`.

## 29. See the graph

`gundb visual` opens a page in your browser that draws everything on the
server as a live graph:

```sh
$ gundb visual
07:12:31 INFO  gundb visual is running, Ctrl-C to stop  page=http://localhost:53122  server=http://localhost:8765/gun
```

Leave it open and replay Part 1. New nodes grow out of their parent with a
green glow, changes flash orange, deletes turn red and fade. Nodes with
encrypted data get a lock, verified signatures a green check. The log at
the bottom lists each PUT, SET, DEL and GET. `--no-open` prints the URL
instead of opening the browser.

---

# When something goes wrong

| You see | It means | Try |
|---|---|---|
| `nothing is listening at http://localhost:8765/gun` | no server there | start `gundb server`, or point `--addr` (or `GUNDB_ADDR`) at one |
| `too many arguments ["Najafi"]` | an unquoted value with a space | `'Ali Najafi'` |
| `… is a top-level node: it holds fields` | `put greeting hello` | `put greeting/text hello` |
| `nothing at users/9` | the path was never written, or was deleted | check with `gundb get <parent> --links` |
| `… is in your own user space (~/), but you are not logged in` | `~/` without `-u`/`-p` | log in, or read someone's space as `~@alias/…` |
| `SEA: unverified data … unsigned data` | writing to another user's space | write to your own `~/…` |
| `was not signed by bob` / `is not signed, so it cannot be from alice` | the value isn't from who `--from` says | check `--from`; the value may be forged |
| `cannot decrypt … with the key you share with alice` | not encrypted for you, or not by alice | check `--from`; it may be for someone else |
| `this device has no group keys for bob` | no `group init` on this device or `--dir` | `gundb group init`, or point `--dir`/`GUNDB_DIR` at your keys |
| `you are not a member of alice/team` | removed, or never added | ask the owner: `gundb group add team <you>` |
| `quota exceeded` | the server's quota is full | delete something (`put <path> null`) |
| `timed out after 2s` | a slow or far server | `--timeout 10s`, or `--timeout 0` to wait forever |

Add `--verbose` to any command to see what goes over the wire.

# Start over

```sh
# terminal 1: Ctrl-C the server, then start it again (in-memory data is gone)
rm -rf /tmp/gundb-scenarios      # group keys from these scenarios
rm -rf ./data                    # if you ran scenario 24
```
