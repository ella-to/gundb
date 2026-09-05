// A browser-style GUN client (the gun/gun build browsers load) driven by argv.
// Prints "RESULT <json>" and exits.
//
//   node client.js <peer> once <soul> [key...]       read a node or field
//   node client.js <peer> put <soul> <json>          put and wait for an ack
//   node client.js <peer> set <soul> <json>          set and wait for an ack
//   node client.js <peer> watch <soul> <key> <want>  .on() until the value equals want
//   node client.js <peer> map <soul> <n>             .map().on() until n items are seen
const WebSocket = require('ws');
const Gun = require('gun/gun');
const [peer, cmd, soul, ...args] = process.argv.slice(2);
const gun = Gun({ peers: [peer], localStorage: false, WebSocket });

const done = (v) => { console.log('RESULT ' + JSON.stringify(v === undefined ? null : v)); setTimeout(() => process.exit(0), 200); };
const strip = (v) => { if (v && typeof v === 'object') { v = Object.assign({}, v); delete v._; } return v; };
setTimeout(() => { console.log('TIMEOUT'); process.exit(2); }, 15000);

let ref = gun.get(soul);
switch (cmd) {
  case 'once':
    args.forEach((k) => { ref = ref.get(k); });
    ref.once((v) => done(strip(v)));
    break;
  case 'put':
    ref.put(JSON.parse(args[0]), (ack) => done(ack.err ? { err: ack.err } : { ok: true }));
    break;
  case 'set':
    ref.set(JSON.parse(args[0]), (ack) => done(ack.err ? { err: ack.err } : { ok: true }));
    break;
  case 'watch':
    console.log('WATCHING');
    ref.get(args[0]).on((v) => { if (v === args[1]) done(v); });
    break;
  case 'map': {
    console.log('WATCHING');
    const seen = {};
    ref.map().on((v, k) => {
      if (v && v.title) seen[k] = v.title;
      if (Object.keys(seen).length >= +args[0]) done(Object.values(seen).sort());
    });
    break;
  }
}
