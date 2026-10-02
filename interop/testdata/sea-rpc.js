// GUN's SEA, driven one JSON request per stdin line; one JSON reply per line.
//   {"op":"sign","data":...,"pair":{...}}   -> {"r": ...}
const SEA = require('gun/sea');
const readline = require('readline');
SEA.throw = false; // failures return undefined, as in normal use
const ops = {
  pair: () => SEA.pair(),
  sign: (q) => SEA.sign(q.data, q.pair),
  verify: (q) => SEA.verify(q.data, q.pub),
  encrypt: (q) => SEA.encrypt(q.data, q.key),
  decrypt: (q) => SEA.decrypt(q.data, q.key),
  secret: (q) => SEA.secret(q.epub, q.pair),
  work: (q) => SEA.work(q.data, q.salt),
  hash: (q) => SEA.work(q.data, null, null, { name: 'SHA-256' }),
};
const rl = readline.createInterface({ input: process.stdin });
let chain = Promise.resolve();
rl.on('line', (line) => {
  chain = chain.then(async () => {
    let out;
    try {
      const q = JSON.parse(line);
      const r = await ops[q.op](q);
      out = { r: r === undefined ? null : r, undef: r === undefined };
    } catch (e) {
      out = { err: String(e && e.message || e) };
    }
    process.stdout.write(JSON.stringify(out) + '\n');
  });
});
