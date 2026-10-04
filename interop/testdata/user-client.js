// A GUN.js client with SEA users, driven by argv. Prints "RESULT <json>".
//
//   node user-client.js <peer> create <alias> <pass>               -> {pub}
//   node user-client.js <peer> put <alias> <pass> <key> <json>     log in, user.get(key).put(json)
//   node user-client.js <peer> login <alias> <pass>                -> {pub}
//   node user-client.js <peer> read <pub> <key> [key...]           gun.user(pub).get(key)... once
const WebSocket = require('ws');
const Gun = require('gun/gun');
require('gun/sea');
const [peer, cmd, ...args] = process.argv.slice(2);
const gun = Gun({ peers: [peer], localStorage: false, WebSocket });

const done = (v) => { console.log('RESULT ' + JSON.stringify(v === undefined ? null : v)); setTimeout(() => process.exit(0), 300); };
const strip = (v) => { if (v && typeof v === 'object') { v = Object.assign({}, v); delete v._; } return v; };
setTimeout(() => { console.log('TIMEOUT'); process.exit(2); }, 20000);

const user = gun.user();
const login = (alias, pass, then) => user.auth(alias, pass, (ack) => ack.err ? done({ err: ack.err }) : then(ack));
switch (cmd) {
  case 'create':
    user.create(args[0], args[1], (ack) => done(ack.err ? { err: ack.err } : { pub: ack.pub }));
    break;
  case 'login':
    login(args[0], args[1], () => done({ pub: user.is.pub }));
    break;
  case 'put':
    login(args[0], args[1], () => user.get(args[2]).put(JSON.parse(args[3]), (ack) => done(ack.err ? { err: ack.err } : { ok: true })));
    break;
  case 'read': {
    let ref = gun.user(args[0]);
    args.slice(1).forEach((k) => { ref = ref.get(k); });
    ref.once((v) => done(strip(v)));
    break;
  }
}
