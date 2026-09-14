// Command loadtest measures how a gundb relay copes with many connected
// users. It starts the relay in a child process, so its CPU and memory are
// measured apart from the load generator. It then connects N simulated
// users and runs these phases:
//
//	idle    N users connected and doing nothing: memory and goroutines per user
//	write   every user puts to its own node and waits for the ack, in a loop
//	read    every user gets a random existing node, in a loop
//	fanout  every user subscribes to one node; one writer updates it at -rate/s
//
// Users are lightweight WebSocket clients that speak GUN's wire protocol, the
// way a browser does, so the numbers describe the relay, not N client DBs.
//
//	go run ./loadtest -users 100,1000,10000
//	go run ./loadtest -users 1000 -store pebble -duration 20s
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ella.to/gundb"
	"ella.to/gundb/storage/pebblestore"
	"ella.to/gundb/transport"
	"ella.to/gundb/transport/ws"
)

var (
	serve      = flag.Bool("serve", false, "internal: run the relay (child process)")
	users      = flag.String("users", "100,1000,10000", "comma-separated user counts to test")
	duration   = flag.Duration("duration", 10*time.Second, "length of the write, read and fanout phases")
	size       = flag.Int("size", 100, "bytes per written value")
	rate       = flag.Int("rate", 10, "fanout: updates per second sent to all subscribers")
	store      = flag.String("store", "memory", "relay store: memory, pebble (synced) or pebble-nosync")
	serverCPUs = flag.Int("server-cpus", runtime.NumCPU()/2, "GOMAXPROCS of the relay")
)

func main() {
	flag.Parse()
	if *serve {
		runServer()
		return
	}
	runtime.GOMAXPROCS(max(1, runtime.NumCPU()-*serverCPUs)) // the relay gets the other cores
	var counts []int
	for s := range strings.SplitSeq(*users, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			log.Fatalf("bad -users %q", *users)
		}
		counts = append(counts, n)
	}
	fmt.Printf("relay: store=%s GOMAXPROCS=%d | load generator: GOMAXPROCS=%d | value size %d B, phases %s\n\n",
		*store, *serverCPUs, runtime.GOMAXPROCS(0), *size, *duration)
	var rows []result
	for _, n := range counts {
		rows = append(rows, run(n))
	}
	summary(rows)
}

// ---- the relay (child process) ----

func runServer() {
	var st gundb.Store
	switch *store {
	case "memory":
	case "pebble", "pebble-nosync":
		dir, err := os.MkdirTemp("", "gundb-loadtest")
		check(err)
		defer os.RemoveAll(dir)
		ps, err := pebblestore.Open(dir, pebblestore.Options{NoSync: *store == "pebble-nosync"})
		check(err)
		defer ps.Close()
		st = ps
	default:
		log.Fatalf("unknown -store %q", *store)
	}
	db := gundb.New(gundb.Options{Store: st})
	defer db.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	mux := http.NewServeMux()
	mux.Handle("/gun", db)
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(readStats(len(db.Peers())))
	})
	fmt.Println("LISTEN", l.Addr())
	go http.Serve(l, mux)
	io.Copy(io.Discard, os.Stdin) // exit when the parent closes stdin
}

type stats struct {
	Peers      int
	Goroutines int
	HeapLive   uint64  // bytes of live heap after a GC
	GoMemory   uint64  // all memory mapped by the Go runtime
	MaxRSS     int64   // peak resident set size
	CPU        float64 // user+system seconds since start
}

func readStats(peers int) stats {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	sample := []metrics.Sample{{Name: "/memory/classes/total:bytes"}}
	metrics.Read(sample)
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	maxRSS := ru.Maxrss
	if runtime.GOOS == "linux" {
		maxRSS *= 1024 // kilobytes on Linux, bytes on macOS
	}
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano()).Seconds()
	return stats{peers, runtime.NumGoroutine(), ms.HeapAlloc, sample[0].Value.Uint64(), maxRSS, cpu}
}

// ---- the load generator ----

type relay struct {
	cmd   *exec.Cmd
	stdin io.Closer
	addr  string
}

func startRelay() *relay {
	exe, err := os.Executable()
	check(err)
	cmd := exec.Command(exe, "-serve", "-store", *store)
	cmd.Env = append(os.Environ(), fmt.Sprintf("GOMAXPROCS=%d", *serverCPUs))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	check(err)
	out, err := cmd.StdoutPipe()
	check(err)
	check(cmd.Start())
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if addr, ok := strings.CutPrefix(sc.Text(), "LISTEN "); ok {
			go io.Copy(io.Discard, out)
			return &relay{cmd: cmd, stdin: stdin, addr: addr}
		}
	}
	log.Fatal("relay did not start: ", sc.Err())
	return nil
}

func (r *relay) stats() stats {
	resp, err := http.Get("http://" + r.addr + "/stats")
	check(err)
	defer resp.Body.Close()
	var s stats
	check(json.NewDecoder(resp.Body).Decode(&s))
	return s
}

func (r *relay) stop() {
	r.stdin.Close()
	r.cmd.Wait()
}

// user is one simulated client connection.
type user struct {
	id   int
	conn transport.Conn
	seq  int

	mu      sync.Mutex
	waiting string        // ID of the request we wait an ack for
	acked   chan struct{} // signalled when it arrives
	onPut   func(put map[string]json.RawMessage)

	sent, recv *atomic.Int64 // wire bytes, shared by all users
}

func (u *user) nextID() string {
	u.seq++
	return "u" + strconv.Itoa(u.id) + "." + strconv.Itoa(u.seq)
}

// readLoop handles every frame from the relay.
func (u *user) readLoop() {
	for {
		frame, err := u.conn.Recv(context.Background())
		if err != nil {
			return
		}
		u.recv.Add(int64(len(frame)))
		var msgs []wire
		if len(frame) > 0 && frame[0] == '[' {
			json.Unmarshal(frame, &msgs)
		} else {
			var m wire
			if json.Unmarshal(frame, &m) == nil {
				msgs = []wire{m}
			}
		}
		for _, m := range msgs {
			u.mu.Lock()
			if m.Ack != "" && m.Ack == u.waiting {
				u.waiting = ""
				u.acked <- struct{}{}
			}
			onPut := u.onPut
			u.mu.Unlock()
			if m.Ack == "" && m.Put != nil && onPut != nil {
				onPut(m.Put)
			}
		}
	}
}

type wire struct {
	Ack string                     `json:"@"`
	Put map[string]json.RawMessage `json:"put"`
}

// request sends msg (built by mk from a fresh ID) and waits for its ack.
func (u *user) request(mk func(id string) []byte) (time.Duration, error) {
	id := u.nextID()
	msg := mk(id)
	u.mu.Lock()
	u.waiting = id
	u.mu.Unlock()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := u.conn.Send(ctx, msg); err != nil {
		return 0, err
	}
	u.sent.Add(int64(len(msg)))
	select {
	case <-u.acked:
		return time.Since(start), nil
	case <-ctx.Done():
		u.mu.Lock()
		u.waiting = ""
		u.mu.Unlock()
		return 0, ctx.Err()
	}
}

func putMsg(id, soul, field, value string, state int64) []byte {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return fmt.Appendf(nil, `{"#":%s,"put":{%s:{"_":{"#":%s,">":{%s:%d}},%s:%s}}}`,
		q(id), q(soul), q(soul), q(field), state, q(field), value)
}

func getMsg(id, soul string) []byte {
	return fmt.Appendf(nil, `{"#":%q,"get":{"#":%q}}`, id, soul)
}

func connect(addr string, first, n int, sent, recv *atomic.Int64) ([]*user, time.Duration) {
	start := time.Now()
	out := make([]*user, n)
	sem := make(chan struct{}, 256)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			var conn transport.Conn
			var err error
			for try := 0; ; try++ {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				conn, err = ws.Dialer{}.Dial(ctx, "ws://"+addr+"/gun")
				cancel()
				if err == nil || try == 5 {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			check(err)
			u := &user{id: first + i, conn: conn, acked: make(chan struct{}, 1), sent: sent, recv: recv}
			go u.readLoop()
			out[i] = u
		})
	}
	wg.Wait()
	return out, time.Since(start)
}

// phase is the outcome of one load phase.
type phase struct {
	ops, errs int
	lat       []time.Duration
	secs      float64
	cpu       float64 // relay CPU cores used on average
	sent      int64   // wire bytes sent by users
	recv      int64   // wire bytes received by users
	peakMem   uint64
}

func (p phase) rate() float64 { return float64(p.ops) / p.secs }

func (p phase) pct(q float64) time.Duration {
	if len(p.lat) == 0 {
		return 0
	}
	return p.lat[min(len(p.lat)-1, int(q*float64(len(p.lat))))]
}

// closedLoop runs op for every user in a loop for the phase duration.
func closedLoop(r *relay, us []*user, sent, recv *atomic.Int64, op func(u *user, rnd *rand.Rand) (time.Duration, error)) phase {
	before := r.stats()
	s0, r0 := sent.Load(), recv.Load()
	deadline := time.Now().Add(*duration)
	var mu sync.Mutex
	var p phase
	var wg sync.WaitGroup
	start := time.Now()
	for _, u := range us {
		wg.Go(func() {
			rnd := rand.New(rand.NewPCG(uint64(u.id), 7))
			var lat []time.Duration
			errs := 0
			for time.Now().Before(deadline) {
				d, err := op(u, rnd)
				if err != nil {
					errs++
					continue
				}
				lat = append(lat, d)
			}
			mu.Lock()
			p.lat = append(p.lat, lat...)
			p.errs += errs
			mu.Unlock()
		})
	}
	wg.Wait()
	p.secs = time.Since(start).Seconds()
	after := r.stats()
	p.ops = len(p.lat)
	p.cpu = (after.CPU - before.CPU) / p.secs
	p.sent, p.recv = sent.Load()-s0, recv.Load()-r0
	p.peakMem = after.GoMemory
	slices.Sort(p.lat)
	return p
}

// fanout subscribes every user to one node and measures how long updates
// take to reach all of them.
func fanout(r *relay, us []*user, writer *user, sent, recv *atomic.Int64) phase {
	const soul = "room"
	if _, err := writer.request(func(id string) []byte {
		return putMsg(id, soul, "t", "0", time.Now().UnixMilli())
	}); err != nil {
		log.Fatal("fanout setup: ", err)
	}
	var mu sync.Mutex
	var p phase
	for _, u := range us {
		u.mu.Lock()
		u.onPut = func(put map[string]json.RawMessage) {
			raw, ok := put[soul]
			if !ok {
				return
			}
			var n struct{ T float64 }
			if json.Unmarshal(raw, &n) != nil || n.T == 0 {
				return
			}
			d := time.Since(time.UnixMicro(int64(n.T)))
			mu.Lock()
			p.lat = append(p.lat, d)
			mu.Unlock()
		}
		u.mu.Unlock()
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 256)
	for _, u := range us { // subscribe: a get marks the soul as wanted
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			u.request(func(id string) []byte { return getMsg(id, soul) })
		})
	}
	wg.Wait()

	before := r.stats()
	s0, r0 := sent.Load(), recv.Load()
	start := time.Now()
	tick := time.NewTicker(time.Second / time.Duration(*rate))
	msgs := 0
	for time.Since(start) < *duration {
		<-tick.C
		now := time.Now()
		if _, err := writer.request(func(id string) []byte {
			return putMsg(id, soul, "t", strconv.FormatInt(now.UnixMicro(), 10), now.UnixMilli())
		}); err != nil {
			p.errs++
		}
		msgs++
	}
	tick.Stop()
	p.secs = time.Since(start).Seconds()
	time.Sleep(time.Second) // let the last updates arrive
	after := r.stats()
	mu.Lock()
	defer mu.Unlock()
	p.ops = len(p.lat)
	p.errs += msgs*len(us) - len(p.lat) // updates that never arrived
	p.cpu = (after.CPU - before.CPU) / time.Since(start).Seconds()
	p.sent, p.recv = sent.Load()-s0, recv.Load()-r0
	p.peakMem = after.GoMemory
	slices.Sort(p.lat)
	for _, u := range us {
		u.mu.Lock()
		u.onPut = nil
		u.mu.Unlock()
	}
	return p
}

type result struct {
	users               int
	connect             time.Duration
	idle                stats
	perUser, perUserGo  float64 // bytes of relay memory per connected user
	write, read, fanout phase
}

func run(n int) result {
	r := startRelay()
	defer r.stop()
	var sent, recv atomic.Int64
	base := r.stats()

	us, took := connect(r.addr, 0, n, &sent, &recv)
	writer, _ := connect(r.addr, n, 1, &sent, &recv) // IDs must not collide: the relay dedups by message ID
	time.Sleep(2 * time.Second)                      // let handshakes settle
	idle := r.stats()
	res := result{users: n, connect: took, idle: idle,
		perUser:   float64(idle.HeapLive-base.HeapLive) / float64(n),
		perUserGo: float64(idle.GoMemory-base.GoMemory) / float64(n)}
	fmt.Printf("== %d users == (relay pprof: http://%s/debug/pprof/)\n", n, r.addr)
	fmt.Printf("connect  %d users in %s; relay holds %d peers, %d goroutines (%.1f per user)\n",
		n, took.Round(time.Millisecond), idle.Peers, idle.Goroutines, float64(idle.Goroutines-base.Goroutines)/float64(n))
	fmt.Printf("idle     relay live heap %s (%s per user), Go memory %s (%s per user)\n",
		mib(idle.HeapLive), kib(res.perUser), mib(idle.GoMemory), kib(res.perUserGo))

	value, _ := json.Marshal(strings.Repeat("x", *size))
	res.write = closedLoop(r, us, &sent, &recv, func(u *user, rnd *rand.Rand) (time.Duration, error) {
		field := "f" + strconv.Itoa(u.seq%5)
		soul := "user" + strconv.Itoa(u.id)
		return u.request(func(id string) []byte {
			return putMsg(id, soul, field, string(value), time.Now().UnixMilli())
		})
	})
	report("write", res.write, int64(*size))

	res.read = closedLoop(r, us, &sent, &recv, func(u *user, rnd *rand.Rand) (time.Duration, error) {
		soul := "user" + strconv.Itoa(rnd.IntN(n))
		return u.request(func(id string) []byte { return getMsg(id, soul) })
	})
	report("read", res.read, int64(5**size))

	res.fanout = fanout(r, us, writer[0], &sent, &recv)
	fmt.Printf("fanout   %d updates/s to %d subscribers: %.0f deliveries/s, %d missed, latency p50 %s p99 %s max %s, relay CPU %.1f cores\n\n",
		*rate, n, res.fanout.rate(), res.fanout.errs, ms(res.fanout.pct(.5)), ms(res.fanout.pct(.99)),
		ms(res.fanout.pct(1)), res.fanout.cpu)

	for _, u := range append(us, writer...) {
		u.conn.Close()
	}
	return res
}

func report(name string, p phase, payload int64) {
	fmt.Printf("%-8s %.0f ops/s (%s/s of values), %d errors, latency p50 %s p95 %s p99 %s max %s, relay CPU %.1f cores, wire in/out %s/s / %s/s\n",
		name, p.rate(), mib(uint64(p.rate()*float64(payload))), p.errs,
		ms(p.pct(.5)), ms(p.pct(.95)), ms(p.pct(.99)), ms(p.pct(1)), p.cpu,
		mib(uint64(float64(p.sent)/p.secs)), mib(uint64(float64(p.recv)/p.secs)))
}

func summary(rows []result) {
	var b bytes.Buffer
	fmt.Fprintln(&b, "| users | relay memory / user | writes/s | write p99 | reads/s | read p99 | fanout deliveries/s | fanout p99 | relay CPU (write) |")
	fmt.Fprintln(&b, "|---|---|---|---|---|---|---|---|---|")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %d | %s | %.0f | %s | %.0f | %s | %.0f | %s | %.1f cores |\n",
			r.users, kib(r.perUserGo), r.write.rate(), ms(r.write.pct(.99)), r.read.rate(), ms(r.read.pct(.99)),
			r.fanout.rate(), ms(r.fanout.pct(.99)), r.write.cpu)
	}
	fmt.Print(b.String())
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000) }
func mib(b uint64) string       { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
func kib(b float64) string      { return fmt.Sprintf("%.1f KiB", b/1024) }

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
