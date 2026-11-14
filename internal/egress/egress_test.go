package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllowed(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34":    true,
		"1.1.1.1":          true,
		"2606:4700::1111":  true,
		"127.0.0.1":        false,
		"127.8.9.10":       false,
		"::1":              false,
		"10.1.2.3":         false,
		"172.16.0.1":       false,
		"172.31.255.255":   false,
		"192.168.1.1":      false,
		"169.254.169.254":  false, // cloud metadata
		"fe80::1":          false,
		"fd00:ec2::254":    false, // AWS metadata over IPv6 (unique local)
		"0.0.0.0":          false,
		"::":               false,
		"100.64.0.1":       false,
		"198.18.0.1":       false,
		"224.0.0.1":        false,
		"255.255.255.255":  false,
		"::ffff:127.0.0.1": false, // IPv4 written as IPv6
		"::ffff:10.0.0.1":  false,
		"::127.0.0.1":      false, // IPv4-compatible
		"64:ff9b::a00:1":   false, // NAT64 for 10.0.0.1
		"2002:a00:1::":     false, // 6to4 for 10.0.0.1
		"2001:db8::1":      false,
	} {
		if got := Allowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", addr, got, want)
		}
	}
	if Allowed(netip.Addr{}) {
		t.Error("the zero Addr is allowed")
	}
}

// fakeResolver answers each lookup with the next entry of answers.
type fakeResolver struct {
	answers [][]netip.Addr
	calls   atomic.Int32
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	n := int(r.calls.Add(1)) - 1
	if n >= len(r.answers) {
		return nil, errors.New("no more answers")
	}
	return r.answers[n], nil
}

// recordingDialer records the addresses it was asked to connect to and
// returns one end of a pipe.
type recordingDialer struct{ dialed []string }

func (d *recordingDialer) dial(_ context.Context, _, address string) (net.Conn, error) {
	d.dialed = append(d.dialed, address)
	c, _ := net.Pipe()
	return c, nil
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

func newTestDialer(r *fakeResolver, rec *recordingDialer) *dialer {
	return &dialer{resolve: r, allow: Allowed, port: Port, dial: rec.dial}
}

// Daniel's condition: one forbidden address refuses the whole name, before
// any connection is made.
func TestDialerRefusesANameIfAnyAddressIsBlocked(t *testing.T) {
	r := &fakeResolver{answers: [][]netip.Addr{addrs("93.184.216.34", "10.0.0.5")}}
	rec := &recordingDialer{}
	_, err := newTestDialer(r, rec).DialContext(context.Background(), "tcp", "hooks.example.com:443")
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want a *BlockedError", err)
	}
	if len(rec.dialed) != 0 {
		t.Errorf("connected to %v before refusing: one bad address must refuse the name", rec.dialed)
	}
}

// The connection goes to the address that was checked: the name is looked
// up once, and the dial is to an IP, never to the name again.
func TestDialerConnectsToTheCheckedAddress(t *testing.T) {
	r := &fakeResolver{answers: [][]netip.Addr{addrs("93.184.216.34")}}
	rec := &recordingDialer{}
	conn, err := newTestDialer(r, rec).DialContext(context.Background(), "tcp", "hooks.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if r.calls.Load() != 1 || len(rec.dialed) != 1 || rec.dialed[0] != "93.184.216.34:443" {
		t.Errorf("lookups %d, dialed %v; want 1 lookup and a dial to 93.184.216.34:443", r.calls.Load(), rec.dialed)
	}
}

// DNS rebinding: the name was public when the job was created and points
// at this machine when the worker connects. Every connection is checked.
func TestDialerChecksEveryConnection(t *testing.T) {
	r := &fakeResolver{answers: [][]netip.Addr{addrs("93.184.216.34"), addrs("127.0.0.1")}}
	rec := &recordingDialer{}
	d := newTestDialer(r, rec)
	if conn, err := d.DialContext(context.Background(), "tcp", "rebind.example.com:443"); err != nil {
		t.Fatalf("first connection: %v", err)
	} else {
		conn.Close()
	}
	_, err := d.DialContext(context.Background(), "tcp", "rebind.example.com:443")
	var blocked *BlockedError
	if !errors.As(err, &blocked) || len(rec.dialed) != 1 {
		t.Errorf("second connection: err %v, dialed %v; want blocked with no second dial", err, rec.dialed)
	}
}

func TestDialerRefusesAddressesAndPortsWithoutLookingUp(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:443", "[::1]:443", "[::ffff:127.0.0.1]:443", "169.254.169.254:443", "10.0.0.1:443",
		"93.184.216.34:80", "hooks.example.com:8443",
	} {
		r := &fakeResolver{}
		rec := &recordingDialer{}
		_, err := newTestDialer(r, rec).DialContext(context.Background(), "tcp", address)
		var blocked *BlockedError
		if !errors.As(err, &blocked) || len(rec.dialed) != 0 || r.calls.Load() != 0 {
			t.Errorf("%s: err %v, dialed %v, lookups %d; want blocked before any lookup or dial", address, err, rec.dialed, r.calls.Load())
		}
	}
}

// The socket refuses a forbidden address even if the code above it didn't.
func TestTheSocketRefusesBlockedAddresses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = netDialer(Allowed).DialContext(context.Background(), "tcp", ln.Addr().String())
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Errorf("dial %s: %v, want a *BlockedError from the socket's check", ln.Addr(), err)
	}
}

// testServer is an HTTPS server on 127.0.0.1 and a client allowed to
// reach it.
func testServer(t *testing.T, h http.Handler, timeout time.Duration) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	roots := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c := NewTestClient(t, TestOptions{
		Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, Port: port, RootCAs: roots, Timeout: timeout,
	})
	return srv, c
}

func TestClientNeverFollowsRedirects(t *testing.T) {
	var followed atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/elsewhere", func(http.ResponseWriter, *http.Request) { followed.Add(1) })
	srv, c := testServer(t, mux, 0)

	resp, err := c.Post(srv.URL+"/hook", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || followed.Load() != 0 {
		t.Errorf("status %d, redirect followed %d times; want the 307 itself and no second request", resp.StatusCode, followed.Load())
	}
}

// A proxy would make the connection for us, to an address nobody checked.
// Checked on the Transport itself: a request to 127.0.0.1 can't show it,
// because http.ProxyFromEnvironment never proxies loopback anyway.
func TestClientNeverUsesAProxy(t *testing.T) {
	if NewClient().Transport.(*http.Transport).Proxy != nil {
		t.Error("NewClient's Transport has a Proxy function: HTTPS_PROXY would connect on our behalf, unchecked")
	}
}

// Daniel's condition: the deadline covers the whole request, reading the
// body included, not only connecting.
func TestTimeoutCoversReadingTheBody(t *testing.T) {
	release := make(chan struct{})
	srv, c := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-release // never finishes the body
	}), 300*time.Millisecond)
	// Registered after the server, so it runs first: srv.Close waits for
	// this handler to return.
	t.Cleanup(func() { close(release) })

	start := time.Now()
	resp, err := c.Get(srv.URL)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("reading a body that never ends succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("gave up after %v, want about 300ms", d)
	}
}

// The production client can't reach the test server at all: wrong port,
// and 127.0.0.1 isn't allowed.
func TestProductionClientRefusesThisMachine(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	_, err := NewClient().Get(srv.URL)
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Errorf("NewClient reached %s: %v", srv.URL, err)
	}
}
