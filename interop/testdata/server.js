// A stock GUN relay, as started by `npx gun`: node server mode with AXE + radisk.
// usage: node server.js <port> <data dir>
const Gun = require('gun');
const http = require('http');
const [port, dir] = process.argv.slice(2);
const server = http.createServer().listen(+port, '127.0.0.1', () => console.log('READY'));
Gun({ web: server, file: dir, multicast: false });
