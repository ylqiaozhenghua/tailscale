// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ipnlocal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	"go4.org/netipx"
	"golang.org/x/net/dns/dnsmessage"
	"tailscale.com/appc"
	"tailscale.com/appc/appctest"
	"tailscale.com/control/controlclient"
	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/interfaces"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/tsd"
	"tailscale.com/tstest"
	"tailscale.com/types/dnstype"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/logid"
	"tailscale.com/types/netmap"
	"tailscale.com/types/opt"
	"tailscale.com/types/ptr"
	"tailscale.com/util/dnsname"
	"tailscale.com/util/mak"
	"tailscale.com/util/must"
	"tailscale.com/util/set"
	"tailscale.com/util/syspolicy"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/filter"
	"tailscale.com/wgengine/wgcfg"
)

func inRemove(ip netip.Addr) bool {
	for _, pfx := range removeFromDefaultRoute {
		if pfx.Contains(ip) {
			return true
		}
	}
	return false
}

func TestShrinkDefaultRoute(t *testing.T) {
	tests := []struct {
		route     string
		in        []string
		out       []string
		localIPFn func(netip.Addr) bool // true if this machine's local IP address should be "in" after shrinking.
	}{
		{
			route: "0.0.0.0/0",
			in:    []string{"1.2.3.4", "25.0.0.1"},
			out: []string{
				"10.0.0.1",
				"10.255.255.255",
				"192.168.0.1",
				"192.168.255.255",
				"172.16.0.1",
				"172.31.255.255",
				"100.101.102.103",
				"224.0.0.1",
				"169.254.169.254",
				// Some random IPv6 stuff that shouldn't be in a v4
				// default route.
				"fe80::",
				"2601::1",
			},
			localIPFn: func(ip netip.Addr) bool { return !inRemove(ip) && ip.Is4() },
		},
		{
			route: "::/0",
			in:    []string{"::1", "2601::1"},
			out: []string{
				"fe80::1",
				"ff00::1",
				tsaddr.TailscaleULARange().Addr().String(),
			},
			localIPFn: func(ip netip.Addr) bool { return !inRemove(ip) && ip.Is6() },
		},
	}

	// Construct a fake local network environment to make this test hermetic.
	// localInterfaceRoutes and hostIPs would normally come from calling interfaceRoutes,
	// and localAddresses would normally come from calling interfaces.LocalAddresses.
	var b netipx.IPSetBuilder
	for _, c := range []string{"127.0.0.0/8", "192.168.9.0/24", "fe80::/32"} {
		p := netip.MustParsePrefix(c)
		b.AddPrefix(p)
	}
	localInterfaceRoutes, err := b.IPSet()
	if err != nil {
		t.Fatal(err)
	}
	hostIPs := []netip.Addr{
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("192.168.9.39"),
		netip.MustParseAddr("fe80::1"),
		netip.MustParseAddr("fe80::437d:feff:feca:49a7"),
	}
	localAddresses := []netip.Addr{
		netip.MustParseAddr("192.168.9.39"),
	}

	for _, test := range tests {
		def := netip.MustParsePrefix(test.route)
		got, err := shrinkDefaultRoute(def, localInterfaceRoutes, hostIPs)
		if err != nil {
			t.Fatalf("shrinkDefaultRoute(%q): %v", test.route, err)
		}
		for _, ip := range test.in {
			if !got.Contains(netip.MustParseAddr(ip)) {
				t.Errorf("shrink(%q).Contains(%v) = false, want true", test.route, ip)
			}
		}
		for _, ip := range test.out {
			if got.Contains(netip.MustParseAddr(ip)) {
				t.Errorf("shrink(%q).Contains(%v) = true, want false", test.route, ip)
			}
		}
		for _, ip := range localAddresses {
			want := test.localIPFn(ip)
			if gotContains := got.Contains(ip); gotContains != want {
				t.Errorf("shrink(%q).Contains(%v) = %v, want %v", test.route, ip, gotContains, want)
			}
		}
	}
}

func TestPeerRoutes(t *testing.T) {
	pp := netip.MustParsePrefix
	tests := []struct {
		name  string
		peers []wgcfg.Peer
		want  []netip.Prefix
	}{
		{
			name: "small_v4",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("100.101.102.103/32"),
					},
				},
			},
			want: []netip.Prefix{
				pp("100.101.102.103/32"),
			},
		},
		{
			name: "big_v4",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("100.101.102.103/32"),
						pp("100.101.102.104/32"),
						pp("100.101.102.105/32"),
					},
				},
			},
			want: []netip.Prefix{
				pp("100.64.0.0/10"),
			},
		},
		{
			name: "has_1_v6",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b240/128"),
					},
				},
			},
			want: []netip.Prefix{
				pp("fd7a:115c:a1e0::/48"),
			},
		},
		{
			name: "has_2_v6",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b240/128"),
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b241/128"),
					},
				},
			},
			want: []netip.Prefix{
				pp("fd7a:115c:a1e0::/48"),
			},
		},
		{
			name: "big_v4_big_v6",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("100.101.102.103/32"),
						pp("100.101.102.104/32"),
						pp("100.101.102.105/32"),
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b240/128"),
						pp("fd7a:115c:a1e0:ab12:4843:cd96:6258:b241/128"),
					},
				},
			},
			want: []netip.Prefix{
				pp("100.64.0.0/10"),
				pp("fd7a:115c:a1e0::/48"),
			},
		},
		{
			name: "output-should-be-sorted",
			peers: []wgcfg.Peer{
				{
					AllowedIPs: []netip.Prefix{
						pp("100.64.0.2/32"),
						pp("10.0.0.0/16"),
					},
				},
				{
					AllowedIPs: []netip.Prefix{
						pp("100.64.0.1/32"),
						pp("10.0.0.0/8"),
					},
				},
			},
			want: []netip.Prefix{
				pp("10.0.0.0/8"),
				pp("10.0.0.0/16"),
				pp("100.64.0.1/32"),
				pp("100.64.0.2/32"),
			},
		},
		{
			name: "skip-unmasked-prefixes",
			peers: []wgcfg.Peer{
				{
					PublicKey: key.NewNode().Public(),
					AllowedIPs: []netip.Prefix{
						pp("100.64.0.2/32"),
						pp("10.0.0.100/16"),
					},
				},
			},
			want: []netip.Prefix{
				pp("100.64.0.2/32"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := peerRoutes(t.Logf, tt.peers, 2)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestPeerAPIBase(t *testing.T) {
	tests := []struct {
		name string
		nm   *netmap.NetworkMap
		peer *tailcfg.Node
		want string
	}{
		{
			name: "nil_netmap",
			peer: new(tailcfg.Node),
			want: "",
		},
		{
			name: "nil_peer",
			nm:   new(netmap.NetworkMap),
			want: "",
		},
		{
			name: "self_only_4_them_both",
			nm: &netmap.NetworkMap{
				SelfNode: (&tailcfg.Node{
					Addresses: []netip.Prefix{
						netip.MustParsePrefix("100.64.1.1/32"),
					},
				}).View(),
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
				Hostinfo: (&tailcfg.Hostinfo{
					Services: []tailcfg.Service{
						{Proto: "peerapi4", Port: 444},
						{Proto: "peerapi6", Port: 666},
					},
				}).View(),
			},
			want: "http://100.64.1.2:444",
		},
		{
			name: "self_only_6_them_both",
			nm: &netmap.NetworkMap{
				SelfNode: (&tailcfg.Node{
					Addresses: []netip.Prefix{
						netip.MustParsePrefix("fe70::1/128"),
					},
				}).View(),
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
				Hostinfo: (&tailcfg.Hostinfo{
					Services: []tailcfg.Service{
						{Proto: "peerapi4", Port: 444},
						{Proto: "peerapi6", Port: 666},
					},
				}).View(),
			},
			want: "http://[fe70::2]:666",
		},
		{
			name: "self_both_them_only_4",
			nm: &netmap.NetworkMap{
				SelfNode: (&tailcfg.Node{
					Addresses: []netip.Prefix{
						netip.MustParsePrefix("100.64.1.1/32"),
						netip.MustParsePrefix("fe70::1/128"),
					},
				}).View(),
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
				Hostinfo: (&tailcfg.Hostinfo{
					Services: []tailcfg.Service{
						{Proto: "peerapi4", Port: 444},
					},
				}).View(),
			},
			want: "http://100.64.1.2:444",
		},
		{
			name: "self_both_them_only_6",
			nm: &netmap.NetworkMap{
				SelfNode: (&tailcfg.Node{
					Addresses: []netip.Prefix{
						netip.MustParsePrefix("100.64.1.1/32"),
						netip.MustParsePrefix("fe70::1/128"),
					},
				}).View(),
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
				Hostinfo: (&tailcfg.Hostinfo{
					Services: []tailcfg.Service{
						{Proto: "peerapi6", Port: 666},
					},
				}).View(),
			},
			want: "http://[fe70::2]:666",
		},
		{
			name: "self_both_them_no_peerapi_service",
			nm: &netmap.NetworkMap{
				SelfNode: (&tailcfg.Node{
					Addresses: []netip.Prefix{
						netip.MustParsePrefix("100.64.1.1/32"),
						netip.MustParsePrefix("fe70::1/128"),
					},
				}).View(),
			},
			peer: &tailcfg.Node{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("100.64.1.2/32"),
					netip.MustParsePrefix("fe70::2/128"),
				},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := peerAPIBase(tt.nm, tt.peer.View())
			if got != tt.want {
				t.Errorf("got %q; want %q", got, tt.want)
			}
		})
	}
}

type panicOnUseTransport struct{}

func (panicOnUseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("unexpected HTTP request")
}

func newTestLocalBackend(t testing.TB) *LocalBackend {
	var logf logger.Logf = logger.Discard
	sys := new(tsd.System)
	store := new(mem.Store)
	sys.Set(store)
	eng, err := wgengine.NewFakeUserspaceEngine(logf, sys.Set)
	if err != nil {
		t.Fatalf("NewFakeUserspaceEngine: %v", err)
	}
	t.Cleanup(eng.Close)
	sys.Set(eng)
	lb, err := NewLocalBackend(logf, logid.PublicID{}, sys, 0)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	return lb
}

// Issue 1573: don't generate a machine key if we don't want to be running.
func TestLazyMachineKeyGeneration(t *testing.T) {
	tstest.Replace(t, &panicOnMachineKeyGeneration, func() bool { return true })

	lb := newTestLocalBackend(t)
	lb.SetHTTPTestClient(&http.Client{
		Transport: panicOnUseTransport{}, // validate we don't send HTTP requests
	})

	if err := lb.Start(ipn.Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the controlclient package goroutines (if they're
	// accidentally started) extra time to schedule and run (and thus
	// hit panicOnUseTransport).
	time.Sleep(500 * time.Millisecond)
}

func TestFileTargets(t *testing.T) {
	b := new(LocalBackend)
	_, err := b.FileTargets()
	if got, want := fmt.Sprint(err), "not connected to the tailnet"; got != want {
		t.Errorf("before connect: got %q; want %q", got, want)
	}

	b.netMap = new(netmap.NetworkMap)
	_, err = b.FileTargets()
	if got, want := fmt.Sprint(err), "not connected to the tailnet"; got != want {
		t.Errorf("non-running netmap: got %q; want %q", got, want)
	}

	b.state = ipn.Running
	_, err = b.FileTargets()
	if got, want := fmt.Sprint(err), "file sharing not enabled by Tailscale admin"; got != want {
		t.Errorf("without cap: got %q; want %q", got, want)
	}

	b.capFileSharing = true
	got, err := b.FileTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unexpected %d peers", len(got))
	}

	var peerMap map[tailcfg.NodeID]tailcfg.NodeView
	mak.NonNil(&peerMap)
	var nodeID tailcfg.NodeID
	nodeID = 1234
	peer := &tailcfg.Node{
		ID:       1234,
		Hostinfo: (&tailcfg.Hostinfo{OS: "tvOS"}).View(),
	}
	peerMap[nodeID] = peer.View()
	b.peers = peerMap
	got, err = b.FileTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unexpected %d peers", len(got))
	}
	// (other cases handled by TestPeerAPIBase above)
}

func TestInternalAndExternalInterfaces(t *testing.T) {
	type interfacePrefix struct {
		i   interfaces.Interface
		pfx netip.Prefix
	}

	masked := func(ips ...interfacePrefix) (pfxs []netip.Prefix) {
		for _, ip := range ips {
			pfxs = append(pfxs, ip.pfx.Masked())
		}
		return pfxs
	}
	iList := func(ips ...interfacePrefix) (il interfaces.List) {
		for _, ip := range ips {
			il = append(il, ip.i)
		}
		return il
	}
	newInterface := func(name, pfx string, wsl2, loopback bool) interfacePrefix {
		ippfx := netip.MustParsePrefix(pfx)
		ip := interfaces.Interface{
			Interface: &net.Interface{},
			AltAddrs: []net.Addr{
				netipx.PrefixIPNet(ippfx),
			},
		}
		if loopback {
			ip.Flags = net.FlagLoopback
		}
		if wsl2 {
			ip.HardwareAddr = []byte{0x00, 0x15, 0x5d, 0x00, 0x00, 0x00}
		}
		return interfacePrefix{i: ip, pfx: ippfx}
	}
	var (
		en0      = newInterface("en0", "10.20.2.5/16", false, false)
		en1      = newInterface("en1", "192.168.1.237/24", false, false)
		wsl      = newInterface("wsl", "192.168.5.34/24", true, false)
		loopback = newInterface("lo0", "127.0.0.1/8", false, true)
	)

	tests := []struct {
		name    string
		goos    string
		il      interfaces.List
		wantInt []netip.Prefix
		wantExt []netip.Prefix
	}{
		{
			name: "single-interface",
			goos: "linux",
			il: iList(
				en0,
				loopback,
			),
			wantInt: masked(loopback),
			wantExt: masked(en0),
		},
		{
			name: "multiple-interfaces",
			goos: "linux",
			il: iList(
				en0,
				en1,
				wsl,
				loopback,
			),
			wantInt: masked(loopback),
			wantExt: masked(en0, en1, wsl),
		},
		{
			name: "wsl2",
			goos: "windows",
			il: iList(
				en0,
				en1,
				wsl,
				loopback,
			),
			wantInt: masked(loopback, wsl),
			wantExt: masked(en0, en1),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotInt, gotExt, err := internalAndExternalInterfacesFrom(tc.il, tc.goos)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotInt, tc.wantInt) {
				t.Errorf("unexpected internal prefixes\ngot %v\nwant %v", gotInt, tc.wantInt)
			}
			if !reflect.DeepEqual(gotExt, tc.wantExt) {
				t.Errorf("unexpected external prefixes\ngot %v\nwant %v", gotExt, tc.wantExt)
			}
		})
	}
}

func TestPacketFilterPermitsUnlockedNodes(t *testing.T) {
	tests := []struct {
		name   string
		peers  []*tailcfg.Node
		filter []filter.Match
		want   bool
	}{
		{
			name: "empty",
			want: false,
		},
		{
			name: "no-unsigned",
			peers: []*tailcfg.Node{
				{ID: 1},
			},
			want: false,
		},
		{
			name: "unsigned-good",
			peers: []*tailcfg.Node{
				{ID: 1, UnsignedPeerAPIOnly: true},
			},
			want: false,
		},
		{
			name: "unsigned-bad",
			peers: []*tailcfg.Node{
				{
					ID:                  1,
					UnsignedPeerAPIOnly: true,
					AllowedIPs: []netip.Prefix{
						netip.MustParsePrefix("100.64.0.0/32"),
					},
				},
			},
			filter: []filter.Match{
				{
					Srcs: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/32")},
					Dsts: []filter.NetPortRange{
						{
							Net: netip.MustParsePrefix("100.99.0.0/32"),
						},
					},
				},
			},
			want: true,
		},
		{
			name: "unsigned-bad-src-is-superset",
			peers: []*tailcfg.Node{
				{
					ID:                  1,
					UnsignedPeerAPIOnly: true,
					AllowedIPs: []netip.Prefix{
						netip.MustParsePrefix("100.64.0.0/32"),
					},
				},
			},
			filter: []filter.Match{
				{
					Srcs: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/24")},
					Dsts: []filter.NetPortRange{
						{
							Net: netip.MustParsePrefix("100.99.0.0/32"),
						},
					},
				},
			},
			want: true,
		},
		{
			name: "unsigned-okay-because-no-dsts",
			peers: []*tailcfg.Node{
				{
					ID:                  1,
					UnsignedPeerAPIOnly: true,
					AllowedIPs: []netip.Prefix{
						netip.MustParsePrefix("100.64.0.0/32"),
					},
				},
			},
			filter: []filter.Match{
				{
					Srcs: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/32")},
					Caps: []filter.CapMatch{
						{
							Dst: netip.MustParsePrefix("100.99.0.0/32"),
							Cap: "foo",
						},
					},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := packetFilterPermitsUnlockedNodes(peersMap(nodeViews(tt.peers)), tt.filter); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStatusWithoutPeers(t *testing.T) {
	b := newTestLocalBackend(t)

	var cc *mockControl
	b.SetControlClientGetterForTesting(func(opts controlclient.Options) (controlclient.Client, error) {
		cc = newClient(t, opts)

		t.Logf("ccGen: new mockControl.")
		cc.called("New")
		return cc, nil
	})
	b.Start(ipn.Options{})
	b.Login(nil)
	cc.send(nil, "", false, &netmap.NetworkMap{
		SelfNode: (&tailcfg.Node{
			MachineAuthorized: true,
			Addresses:         ipps("100.101.101.101"),
		}).View(),
	})
	got := b.StatusWithoutPeers()
	if got.TailscaleIPs == nil {
		t.Errorf("got nil, expected TailscaleIPs value to not be nil")
	}
	if !reflect.DeepEqual(got.TailscaleIPs, got.Self.TailscaleIPs) {
		t.Errorf("got %v, expected %v", got.TailscaleIPs, got.Self.TailscaleIPs)
	}
}

// legacyBackend was the interface between Tailscale frontends
// (e.g. cmd/tailscale, iOS/MacOS/Windows GUIs) and the tailscale
// backend (e.g. cmd/tailscaled) running on the same machine.
// (It has nothing to do with the interface between the backends
// and the cloud control plane.)
type legacyBackend interface {
	// SetNotifyCallback sets the callback to be called on updates
	// from the backend to the client.
	SetNotifyCallback(func(ipn.Notify))
	// Start starts or restarts the backend, typically when a
	// frontend client connects.
	Start(ipn.Options) error
	// StartLoginInteractive requests to start a new interactive login
	// flow. This should trigger a new BrowseToURL notification
	// eventually.
	StartLoginInteractive()
	// Login logs in with an OAuth2 token.
	Login(token *tailcfg.Oauth2Token)
	// SetPrefs installs a new set of user preferences, including
	// WantRunning. This may cause the wireguard engine to
	// reconfigure or stop.
	SetPrefs(*ipn.Prefs)
	// RequestEngineStatus polls for an update from the wireguard
	// engine. Only needed if you want to display byte
	// counts. Connection events are emitted automatically without
	// polling.
	RequestEngineStatus()
}

// Verify that LocalBackend still implements the legacyBackend interface
// for now, at least until the macOS and iOS clients move off of it.
var _ legacyBackend = (*LocalBackend)(nil)

func TestWatchNotificationsCallbacks(t *testing.T) {
	b := new(LocalBackend)
	// activeWatchSessions is typically set in NewLocalBackend
	// so WatchNotifications expects it to be non-empty.
	b.activeWatchSessions = make(set.Set[string])
	n := new(ipn.Notify)
	b.WatchNotifications(context.Background(), 0, func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		// Ensure a watcher has been installed.
		if len(b.notifyWatchers) != 1 {
			t.Fatalf("unexpected number of watchers in new LocalBackend, want: 1 got: %v", len(b.notifyWatchers))
		}
		// Send a notification. Range over notifyWatchers to get the channel
		// because WatchNotifications doesn't expose the handle for it.
		for _, sess := range b.notifyWatchers {
			select {
			case sess.ch <- n:
			default:
				t.Fatalf("could not send notification")
			}
		}
	}, func(roNotify *ipn.Notify) bool {
		if roNotify != n {
			t.Fatalf("unexpected notification received. want: %v got: %v", n, roNotify)
		}
		return false
	})

	// Ensure watchers have been cleaned up.
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.notifyWatchers) != 0 {
		t.Fatalf("unexpected number of watchers in new LocalBackend, want: 0 got: %v", len(b.notifyWatchers))
	}
}

// tests LocalBackend.updateNetmapDeltaLocked
func TestUpdateNetmapDelta(t *testing.T) {
	b := newTestLocalBackend(t)
	if b.updateNetmapDeltaLocked(nil) {
		t.Errorf("updateNetmapDeltaLocked() = true, want false with nil netmap")
	}

	b.netMap = &netmap.NetworkMap{}
	for i := 0; i < 5; i++ {
		b.netMap.Peers = append(b.netMap.Peers, (&tailcfg.Node{ID: (tailcfg.NodeID(i) + 1)}).View())
	}
	b.updatePeersFromNetmapLocked(b.netMap)

	someTime := time.Unix(123, 0)
	muts, ok := netmap.MutationsFromMapResponse(&tailcfg.MapResponse{
		PeersChangedPatch: []*tailcfg.PeerChange{
			{
				NodeID:     1,
				DERPRegion: 1,
			},
			{
				NodeID: 2,
				Online: ptr.To(true),
			},
			{
				NodeID: 3,
				Online: ptr.To(false),
			},
			{
				NodeID:   4,
				LastSeen: ptr.To(someTime),
			},
		},
	}, someTime)
	if !ok {
		t.Fatal("netmap.MutationsFromMapResponse failed")
	}

	if !b.updateNetmapDeltaLocked(muts) {
		t.Fatalf("updateNetmapDeltaLocked() = false, want true with new netmap")
	}

	wants := []*tailcfg.Node{
		{
			ID:   1,
			DERP: "127.3.3.40:1",
		},
		{
			ID:     2,
			Online: ptr.To(true),
		},
		{
			ID:     3,
			Online: ptr.To(false),
		},
		{
			ID:       4,
			LastSeen: ptr.To(someTime),
		},
	}
	for _, want := range wants {
		gotv, ok := b.peers[want.ID]
		if !ok {
			t.Errorf("netmap.Peer %v missing from b.peers", want.ID)
			continue
		}
		got := gotv.AsStruct()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("netmap.Peer %v wrong.\n got: %v\nwant: %v", want.ID, logger.AsJSON(got), logger.AsJSON(want))
		}
	}
}

// tests WhoIs and indirectly that setNetMapLocked updates b.nodeByAddr correctly.
func TestWhoIs(t *testing.T) {
	b := newTestLocalBackend(t)
	b.setNetMapLocked(&netmap.NetworkMap{
		SelfNode: (&tailcfg.Node{
			ID:        1,
			User:      10,
			Addresses: []netip.Prefix{netip.MustParsePrefix("100.101.102.103/32")},
		}).View(),
		Peers: []tailcfg.NodeView{
			(&tailcfg.Node{
				ID:        2,
				User:      20,
				Addresses: []netip.Prefix{netip.MustParsePrefix("100.200.200.200/32")},
			}).View(),
		},
		UserProfiles: map[tailcfg.UserID]tailcfg.UserProfile{
			10: {
				DisplayName: "Myself",
			},
			20: {
				DisplayName: "Peer",
			},
		},
	})
	tests := []struct {
		q        string
		want     tailcfg.NodeID // 0 means want ok=false
		wantName string
	}{
		{"100.101.102.103:0", 1, "Myself"},
		{"100.101.102.103:123", 1, "Myself"},
		{"100.200.200.200:0", 2, "Peer"},
		{"100.200.200.200:123", 2, "Peer"},
		{"100.4.0.4:404", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.q, func(t *testing.T) {
			nv, up, ok := b.WhoIs(netip.MustParseAddrPort(tt.q))
			var got tailcfg.NodeID
			if ok {
				got = nv.ID()
			}
			if got != tt.want {
				t.Errorf("got nodeID %v; want %v", got, tt.want)
			}
			if up.DisplayName != tt.wantName {
				t.Errorf("got name %q; want %q", up.DisplayName, tt.wantName)
			}
		})
	}
}

func TestWireguardExitNodeDNSResolvers(t *testing.T) {
	type tc struct {
		name          string
		id            tailcfg.StableNodeID
		peers         []*tailcfg.Node
		wantOK        bool
		wantResolvers []*dnstype.Resolver
	}

	tests := []tc{
		{
			name:          "no peers",
			id:            "1",
			wantOK:        false,
			wantResolvers: nil,
		},
		{
			name: "non wireguard peer",
			id:   "1",
			peers: []*tailcfg.Node{
				{
					ID:                   1,
					StableID:             "1",
					IsWireGuardOnly:      false,
					ExitNodeDNSResolvers: []*dnstype.Resolver{{Addr: "dns.example.com"}},
				},
			},
			wantOK:        false,
			wantResolvers: nil,
		},
		{
			name: "no matching IDs",
			id:   "2",
			peers: []*tailcfg.Node{
				{
					ID:                   1,
					StableID:             "1",
					IsWireGuardOnly:      true,
					ExitNodeDNSResolvers: []*dnstype.Resolver{{Addr: "dns.example.com"}},
				},
			},
			wantOK:        false,
			wantResolvers: nil,
		},
		{
			name: "wireguard peer",
			id:   "1",
			peers: []*tailcfg.Node{
				{
					ID:                   1,
					StableID:             "1",
					IsWireGuardOnly:      true,
					ExitNodeDNSResolvers: []*dnstype.Resolver{{Addr: "dns.example.com"}},
				},
			},
			wantOK:        true,
			wantResolvers: []*dnstype.Resolver{{Addr: "dns.example.com"}},
		},
	}

	for _, tc := range tests {
		peers := peersMap(nodeViews(tc.peers))
		nm := &netmap.NetworkMap{}
		gotResolvers, gotOK := wireguardExitNodeDNSResolvers(nm, peers, tc.id)

		if gotOK != tc.wantOK || !resolversEqual(t, gotResolvers, tc.wantResolvers) {
			t.Errorf("case: %s: got %v, %v, want %v, %v", tc.name, gotOK, gotResolvers, tc.wantOK, tc.wantResolvers)
		}
	}
}

func TestDNSConfigForNetmapForExitNodeConfigs(t *testing.T) {
	type tc struct {
		name                 string
		exitNode             tailcfg.StableNodeID
		peers                []tailcfg.NodeView
		dnsConfig            *tailcfg.DNSConfig
		wantDefaultResolvers []*dnstype.Resolver
		wantRoutes           map[dnsname.FQDN][]*dnstype.Resolver
	}

	defaultResolvers := []*dnstype.Resolver{{Addr: "default.example.com"}}
	wgResolvers := []*dnstype.Resolver{{Addr: "wg.example.com"}}
	peers := []tailcfg.NodeView{
		(&tailcfg.Node{
			ID:                   1,
			StableID:             "wg",
			IsWireGuardOnly:      true,
			ExitNodeDNSResolvers: wgResolvers,
			Hostinfo:             (&tailcfg.Hostinfo{}).View(),
		}).View(),
		// regular tailscale exit node with DNS capabilities
		(&tailcfg.Node{
			Cap:      26,
			ID:       2,
			StableID: "ts",
			Hostinfo: (&tailcfg.Hostinfo{}).View(),
		}).View(),
	}
	exitDOH := peerAPIBase(&netmap.NetworkMap{Peers: peers}, peers[0]) + "/dns-query"
	routes := map[dnsname.FQDN][]*dnstype.Resolver{
		"route.example.com.": {{Addr: "route.example.com"}},
	}
	stringifyRoutes := func(routes map[dnsname.FQDN][]*dnstype.Resolver) map[string][]*dnstype.Resolver {
		if routes == nil {
			return nil
		}
		m := make(map[string][]*dnstype.Resolver)
		for k, v := range routes {
			m[string(k)] = v
		}
		return m
	}

	tests := []tc{
		{
			name:                 "noExit/noRoutes/noResolver",
			exitNode:             "",
			peers:                peers,
			dnsConfig:            &tailcfg.DNSConfig{},
			wantDefaultResolvers: nil,
			wantRoutes:           nil,
		},
		{
			name:                 "tsExit/noRoutes/noResolver",
			exitNode:             "ts",
			peers:                peers,
			dnsConfig:            &tailcfg.DNSConfig{},
			wantDefaultResolvers: []*dnstype.Resolver{{Addr: exitDOH}},
			wantRoutes:           nil,
		},
		{
			name:                 "tsExit/noRoutes/defaultResolver",
			exitNode:             "ts",
			peers:                peers,
			dnsConfig:            &tailcfg.DNSConfig{Resolvers: defaultResolvers},
			wantDefaultResolvers: []*dnstype.Resolver{{Addr: exitDOH}},
			wantRoutes:           nil,
		},

		// The following two cases may need to be revisited. For a shared-in
		// exit node split-DNS may effectively break, furthermore in the future
		// if different nodes observe different DNS configurations, even a
		// tailnet local exit node may present a different DNS configuration,
		// which may not meet expectations in some use cases.
		// In the case where a default resolver is set, the default resolver
		// should also perhaps take precedence also.
		{
			name:                 "tsExit/routes/noResolver",
			exitNode:             "ts",
			peers:                peers,
			dnsConfig:            &tailcfg.DNSConfig{Routes: stringifyRoutes(routes)},
			wantDefaultResolvers: []*dnstype.Resolver{{Addr: exitDOH}},
			wantRoutes:           nil,
		},
		{
			name:                 "tsExit/routes/defaultResolver",
			exitNode:             "ts",
			peers:                peers,
			dnsConfig:            &tailcfg.DNSConfig{Routes: stringifyRoutes(routes), Resolvers: defaultResolvers},
			wantDefaultResolvers: []*dnstype.Resolver{{Addr: exitDOH}},
			wantRoutes:           nil,
		},

		// WireGuard exit nodes with DNS capabilities provide a "fallback" type
		// behavior, they have a lower precedence than a default resolver, but
		// otherwise allow split-DNS to operate as normal, and are used when
		// there is no default resolver.
		{
			name:                 "wgExit/noRoutes/noResolver",
			exitNode:             "wg",
			peers:                peers,
			dnsConfig:            &tailcfg.DNSConfig{},
			wantDefaultResolvers: wgResolvers,
			wantRoutes:           nil,
		},
		{
			name:                 "wgExit/noRoutes/defaultResolver",
			exitNode:             "wg",
			peers:                peers,
			dnsConfig:            &tailcfg.DNSConfig{Resolvers: defaultResolvers},
			wantDefaultResolvers: defaultResolvers,
			wantRoutes:           nil,
		},
		{
			name:                 "wgExit/routes/defaultResolver",
			exitNode:             "wg",
			peers:                peers,
			dnsConfig:            &tailcfg.DNSConfig{Routes: stringifyRoutes(routes), Resolvers: defaultResolvers},
			wantDefaultResolvers: defaultResolvers,
			wantRoutes:           routes,
		},
		{
			name:                 "wgExit/routes/noResolver",
			exitNode:             "wg",
			peers:                peers,
			dnsConfig:            &tailcfg.DNSConfig{Routes: stringifyRoutes(routes)},
			wantDefaultResolvers: wgResolvers,
			wantRoutes:           routes,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nm := &netmap.NetworkMap{
				Peers: tc.peers,
				DNS:   *tc.dnsConfig,
			}

			prefs := &ipn.Prefs{ExitNodeID: tc.exitNode, CorpDNS: true}
			got := dnsConfigForNetmap(nm, peersMap(tc.peers), prefs.View(), t.Logf, "")
			if !resolversEqual(t, got.DefaultResolvers, tc.wantDefaultResolvers) {
				t.Errorf("DefaultResolvers: got %#v, want %#v", got.DefaultResolvers, tc.wantDefaultResolvers)
			}
			if !routesEqual(t, got.Routes, tc.wantRoutes) {
				t.Errorf("Routes: got %#v, want %#v", got.Routes, tc.wantRoutes)
			}
		})
	}
}

func TestOfferingAppConnector(t *testing.T) {
	b := newTestBackend(t)
	if b.OfferingAppConnector() {
		t.Fatal("unexpected offering app connector")
	}
	b.appConnector = appc.NewAppConnector(t.Logf, nil)
	if !b.OfferingAppConnector() {
		t.Fatal("unexpected not offering app connector")
	}
}

func TestRouteAdvertiser(t *testing.T) {
	b := newTestBackend(t)
	testPrefix := netip.MustParsePrefix("192.0.0.8/32")

	ra := appc.RouteAdvertiser(b)
	must.Do(ra.AdvertiseRoute(testPrefix))

	routes := b.Prefs().AdvertiseRoutes()
	if routes.Len() != 1 || routes.At(0) != testPrefix {
		t.Fatalf("got routes %v, want %v", routes, []netip.Prefix{testPrefix})
	}

	must.Do(ra.UnadvertiseRoute(testPrefix))

	routes = b.Prefs().AdvertiseRoutes()
	if routes.Len() != 0 {
		t.Fatalf("got routes %v, want none", routes)
	}
}

func TestRouterAdvertiserIgnoresContainedRoutes(t *testing.T) {
	b := newTestBackend(t)
	testPrefix := netip.MustParsePrefix("192.0.0.0/24")
	ra := appc.RouteAdvertiser(b)
	must.Do(ra.AdvertiseRoute(testPrefix))

	routes := b.Prefs().AdvertiseRoutes()
	if routes.Len() != 1 || routes.At(0) != testPrefix {
		t.Fatalf("got routes %v, want %v", routes, []netip.Prefix{testPrefix})
	}

	must.Do(ra.AdvertiseRoute(netip.MustParsePrefix("192.0.0.8/32")))

	// the above /32 is not added as it is contained within the /24
	routes = b.Prefs().AdvertiseRoutes()
	if routes.Len() != 1 || routes.At(0) != testPrefix {
		t.Fatalf("got routes %v, want %v", routes, []netip.Prefix{testPrefix})
	}
}

func TestObserveDNSResponse(t *testing.T) {
	b := newTestBackend(t)

	// ensure no error when no app connector is configured
	b.ObserveDNSResponse(dnsResponse("example.com.", "192.0.0.8"))

	rc := &appctest.RouteCollector{}
	b.appConnector = appc.NewAppConnector(t.Logf, rc)
	b.appConnector.UpdateDomains([]string{"example.com"})
	b.appConnector.Wait(context.Background())

	b.ObserveDNSResponse(dnsResponse("example.com.", "192.0.0.8"))
	b.appConnector.Wait(context.Background())
	wantRoutes := []netip.Prefix{netip.MustParsePrefix("192.0.0.8/32")}
	if !slices.Equal(rc.Routes(), wantRoutes) {
		t.Fatalf("got routes %v, want %v", rc.Routes(), wantRoutes)
	}
}

func TestCoveredRouteRangeNoDefault(t *testing.T) {
	tests := []struct {
		existingRoute netip.Prefix
		newRoute      netip.Prefix
		want          bool
	}{
		{
			existingRoute: netip.MustParsePrefix("192.0.0.1/32"),
			newRoute:      netip.MustParsePrefix("192.0.0.1/32"),
			want:          true,
		},
		{
			existingRoute: netip.MustParsePrefix("192.0.0.1/32"),
			newRoute:      netip.MustParsePrefix("192.0.0.2/32"),
			want:          false,
		},
		{
			existingRoute: netip.MustParsePrefix("192.0.0.0/24"),
			newRoute:      netip.MustParsePrefix("192.0.0.1/32"),
			want:          true,
		},
		{
			existingRoute: netip.MustParsePrefix("192.0.0.0/16"),
			newRoute:      netip.MustParsePrefix("192.0.0.0/24"),
			want:          true,
		},
		{
			existingRoute: netip.MustParsePrefix("0.0.0.0/0"),
			newRoute:      netip.MustParsePrefix("192.0.0.0/24"),
			want:          false,
		},
		{
			existingRoute: netip.MustParsePrefix("::/0"),
			newRoute:      netip.MustParsePrefix("2001:db8::/32"),
			want:          false,
		},
	}

	for _, tt := range tests {
		got := coveredRouteRangeNoDefault([]netip.Prefix{tt.existingRoute}, tt.newRoute)
		if got != tt.want {
			t.Errorf("coveredRouteRange(%v, %v) = %v, want %v", tt.existingRoute, tt.newRoute, got, tt.want)
		}
	}
}

func TestReconfigureAppConnector(t *testing.T) {
	b := newTestBackend(t)
	b.reconfigAppConnectorLocked(b.netMap, b.pm.prefs)
	if b.appConnector != nil {
		t.Fatal("unexpected app connector")
	}

	b.EditPrefs(&ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			AppConnector: ipn.AppConnectorPrefs{
				Advertise: true,
			},
		},
		AppConnectorSet: true,
	})
	b.reconfigAppConnectorLocked(b.netMap, b.pm.prefs)
	if b.appConnector == nil {
		t.Fatal("expected app connector")
	}

	appCfg := `{
		"name": "example",
		"domains": ["example.com"],
		"connectors": ["tag:example"]
	}`

	b.netMap.SelfNode = (&tailcfg.Node{
		Name: "example.ts.net",
		Tags: []string{"tag:example"},
		CapMap: (tailcfg.NodeCapMap)(map[tailcfg.NodeCapability][]tailcfg.RawMessage{
			"tailscale.com/app-connectors": {tailcfg.RawMessage(appCfg)},
		}),
	}).View()

	b.reconfigAppConnectorLocked(b.netMap, b.pm.prefs)
	b.appConnector.Wait(context.Background())

	want := []string{"example.com"}
	if !slices.Equal(b.appConnector.Domains().AsSlice(), want) {
		t.Fatalf("got domains %v, want %v", b.appConnector.Domains(), want)
	}
	if v, _ := b.hostinfo.AppConnector.Get(); !v {
		t.Fatalf("expected app connector service")
	}

	// disable the connector in order to assert that the service is removed
	b.EditPrefs(&ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			AppConnector: ipn.AppConnectorPrefs{
				Advertise: false,
			},
		},
		AppConnectorSet: true,
	})
	b.reconfigAppConnectorLocked(b.netMap, b.pm.prefs)
	if b.appConnector != nil {
		t.Fatal("expected no app connector")
	}
	if v, _ := b.hostinfo.AppConnector.Get(); v {
		t.Fatalf("expected no app connector service")
	}
}

func resolversEqual(t *testing.T, a, b []*dnstype.Resolver) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		t.Errorf("resolversEqual: a == nil || b == nil : %#v != %#v", a, b)
		return false
	}
	if len(a) != len(b) {
		t.Errorf("resolversEqual: len(a) != len(b) : %#v != %#v", a, b)
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			t.Errorf("resolversEqual: a != b [%d]: %v != %v", i, *a[i], *b[i])
			return false
		}
	}
	return true
}

func routesEqual(t *testing.T, a, b map[dnsname.FQDN][]*dnstype.Resolver) bool {
	if len(a) != len(b) {
		t.Logf("routes: len(a) != len(b): %d != %d", len(a), len(b))
		return false
	}
	for name := range a {
		if !resolversEqual(t, a[name], b[name]) {
			t.Logf("routes: a != b [%s]: %v != %v", name, a[name], b[name])
			return false
		}
	}
	return true
}

// dnsResponse is a test helper that creates a DNS response buffer for the given domain and address
func dnsResponse(domain, address string) []byte {
	addr := netip.MustParseAddr(address)
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	b.EnableCompression()
	b.StartAnswers()
	switch addr.BitLen() {
	case 32:
		b.AResource(
			dnsmessage.ResourceHeader{
				Name:  dnsmessage.MustNewName(domain),
				Type:  dnsmessage.TypeA,
				Class: dnsmessage.ClassINET,
				TTL:   0,
			},
			dnsmessage.AResource{
				A: addr.As4(),
			},
		)
	case 128:
		b.AAAAResource(
			dnsmessage.ResourceHeader{
				Name:  dnsmessage.MustNewName(domain),
				Type:  dnsmessage.TypeAAAA,
				Class: dnsmessage.ClassINET,
				TTL:   0,
			},
			dnsmessage.AAAAResource{
				AAAA: addr.As16(),
			},
		)
	default:
		panic("invalid address length")
	}
	return must.Get(b.Finish())
}

type errorSyspolicyHandler struct {
	t         *testing.T
	err       error
	key       syspolicy.Key
	allowKeys map[syspolicy.Key]*string
}

func (h *errorSyspolicyHandler) ReadString(key string) (string, error) {
	sk := syspolicy.Key(key)
	if _, ok := h.allowKeys[sk]; !ok {
		h.t.Errorf("ReadString: %q is not in list of permitted keys", h.key)
	}
	if sk == h.key {
		return "", h.err
	}
	return "", syspolicy.ErrNoSuchKey
}

func (h *errorSyspolicyHandler) ReadUInt64(key string) (uint64, error) {
	h.t.Errorf("ReadUInt64(%q) unexpectedly called", key)
	return 0, syspolicy.ErrNoSuchKey
}

func (h *errorSyspolicyHandler) ReadBoolean(key string) (bool, error) {
	h.t.Errorf("ReadBoolean(%q) unexpectedly called", key)
	return false, syspolicy.ErrNoSuchKey
}

type mockSyspolicyHandler struct {
	t *testing.T
	// stringPolicies is the collection of policies that we expect to see
	// queried by the current test. If the policy is expected but unset, then
	// use nil, otherwise use a string equal to the policy's desired value.
	stringPolicies map[syspolicy.Key]*string
	// failUnknownPolicies is set if policies other than those in stringPolicies
	// (uint64 or bool policies are not supported by mockSyspolicyHandler yet)
	// should be considered a test failure if they are queried.
	failUnknownPolicies bool
}

func (h *mockSyspolicyHandler) ReadString(key string) (string, error) {
	if s, ok := h.stringPolicies[syspolicy.Key(key)]; ok {
		if s == nil {
			return "", syspolicy.ErrNoSuchKey
		}
		return *s, nil
	}
	if h.failUnknownPolicies {
		h.t.Errorf("ReadString(%q) unexpectedly called", key)
	}
	return "", syspolicy.ErrNoSuchKey
}

func (h *mockSyspolicyHandler) ReadUInt64(key string) (uint64, error) {
	if h.failUnknownPolicies {
		h.t.Errorf("ReadUInt64(%q) unexpectedly called", key)
	}
	return 0, syspolicy.ErrNoSuchKey
}

func (h *mockSyspolicyHandler) ReadBoolean(key string) (bool, error) {
	if h.failUnknownPolicies {
		h.t.Errorf("ReadBoolean(%q) unexpectedly called", key)
	}
	return false, syspolicy.ErrNoSuchKey
}

func TestSetExitNodeIDPolicy(t *testing.T) {
	pfx := netip.MustParsePrefix
	tests := []struct {
		name           string
		exitNodeIPKey  bool
		exitNodeIDKey  bool
		exitNodeID     string
		exitNodeIP     string
		prefs          *ipn.Prefs
		exitNodeIPWant string
		exitNodeIDWant string
		prefsChanged   bool
		nm             *netmap.NetworkMap
	}{
		{
			name:           "ExitNodeID key is set",
			exitNodeIDKey:  true,
			exitNodeID:     "123",
			exitNodeIDWant: "123",
			prefsChanged:   true,
		},
		{
			name:           "ExitNodeID key not set",
			exitNodeIDKey:  true,
			exitNodeIDWant: "",
			prefsChanged:   false,
		},
		{
			name:           "ExitNodeID key set, ExitNodeIP preference set",
			exitNodeIDKey:  true,
			exitNodeID:     "123",
			prefs:          &ipn.Prefs{ExitNodeIP: netip.MustParseAddr("127.0.0.1")},
			exitNodeIDWant: "123",
			prefsChanged:   true,
		},
		{
			name:           "ExitNodeID key not set, ExitNodeIP key set",
			exitNodeIPKey:  true,
			exitNodeIP:     "127.0.0.1",
			prefs:          &ipn.Prefs{ExitNodeIP: netip.MustParseAddr("127.0.0.1")},
			exitNodeIPWant: "127.0.0.1",
			prefsChanged:   false,
		},
		{
			name:           "ExitNodeIP key set, existing ExitNodeIP pref",
			exitNodeIPKey:  true,
			exitNodeIP:     "127.0.0.1",
			prefs:          &ipn.Prefs{ExitNodeIP: netip.MustParseAddr("127.0.0.1")},
			exitNodeIPWant: "127.0.0.1",
			prefsChanged:   false,
		},
		{
			name:           "existing preferences match policy",
			exitNodeIDKey:  true,
			exitNodeID:     "123",
			prefs:          &ipn.Prefs{ExitNodeID: tailcfg.StableNodeID("123")},
			exitNodeIDWant: "123",
			prefsChanged:   false,
		},
		{
			name:           "ExitNodeIP set if net map does not have corresponding node",
			exitNodeIPKey:  true,
			prefs:          &ipn.Prefs{ExitNodeIP: netip.MustParseAddr("127.0.0.1")},
			exitNodeIP:     "127.0.0.1",
			exitNodeIPWant: "127.0.0.1",
			prefsChanged:   false,
			nm: &netmap.NetworkMap{
				Name: "foo.tailnet",
				SelfNode: (&tailcfg.Node{
					Addresses: []netip.Prefix{
						pfx("100.102.103.104/32"),
						pfx("100::123/128"),
					},
				}).View(),
				Peers: []tailcfg.NodeView{
					(&tailcfg.Node{
						Name: "a.tailnet",
						Addresses: []netip.Prefix{
							pfx("100.0.0.201/32"),
							pfx("100::201/128"),
						},
					}).View(),
					(&tailcfg.Node{
						Name: "b.tailnet",
						Addresses: []netip.Prefix{
							pfx("100::202/128"),
						},
					}).View(),
				},
			},
		},
		{
			name:           "ExitNodeIP cleared if net map has corresponding node - policy matches prefs",
			prefs:          &ipn.Prefs{ExitNodeIP: netip.MustParseAddr("127.0.0.1")},
			exitNodeIPKey:  true,
			exitNodeIP:     "127.0.0.1",
			exitNodeIPWant: "",
			exitNodeIDWant: "123",
			prefsChanged:   true,
			nm: &netmap.NetworkMap{
				Name: "foo.tailnet",
				SelfNode: (&tailcfg.Node{
					Addresses: []netip.Prefix{
						pfx("100.102.103.104/32"),
						pfx("100::123/128"),
					},
				}).View(),
				Peers: []tailcfg.NodeView{
					(&tailcfg.Node{
						Name:     "a.tailnet",
						StableID: tailcfg.StableNodeID("123"),
						Addresses: []netip.Prefix{
							pfx("127.0.0.1/32"),
							pfx("100::201/128"),
						},
					}).View(),
					(&tailcfg.Node{
						Name: "b.tailnet",
						Addresses: []netip.Prefix{
							pfx("100::202/128"),
						},
					}).View(),
				},
			},
		},
		{
			name:           "ExitNodeIP cleared if net map has corresponding node - no policy set",
			prefs:          &ipn.Prefs{ExitNodeIP: netip.MustParseAddr("127.0.0.1")},
			exitNodeIPWant: "",
			exitNodeIDWant: "123",
			prefsChanged:   true,
			nm: &netmap.NetworkMap{
				Name: "foo.tailnet",
				SelfNode: (&tailcfg.Node{
					Addresses: []netip.Prefix{
						pfx("100.102.103.104/32"),
						pfx("100::123/128"),
					},
				}).View(),
				Peers: []tailcfg.NodeView{
					(&tailcfg.Node{
						Name:     "a.tailnet",
						StableID: tailcfg.StableNodeID("123"),
						Addresses: []netip.Prefix{
							pfx("127.0.0.1/32"),
							pfx("100::201/128"),
						},
					}).View(),
					(&tailcfg.Node{
						Name: "b.tailnet",
						Addresses: []netip.Prefix{
							pfx("100::202/128"),
						},
					}).View(),
				},
			},
		},
		{
			name:           "ExitNodeIP cleared if net map has corresponding node - different exit node IP in policy",
			exitNodeIPKey:  true,
			prefs:          &ipn.Prefs{ExitNodeIP: netip.MustParseAddr("127.0.0.1")},
			exitNodeIP:     "100.64.5.6",
			exitNodeIPWant: "",
			exitNodeIDWant: "123",
			prefsChanged:   true,
			nm: &netmap.NetworkMap{
				Name: "foo.tailnet",
				SelfNode: (&tailcfg.Node{
					Addresses: []netip.Prefix{
						pfx("100.102.103.104/32"),
						pfx("100::123/128"),
					},
				}).View(),
				Peers: []tailcfg.NodeView{
					(&tailcfg.Node{
						Name:     "a.tailnet",
						StableID: tailcfg.StableNodeID("123"),
						Addresses: []netip.Prefix{
							pfx("100.64.5.6/32"),
							pfx("100::201/128"),
						},
					}).View(),
					(&tailcfg.Node{
						Name: "b.tailnet",
						Addresses: []netip.Prefix{
							pfx("100::202/128"),
						},
					}).View(),
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b := newTestBackend(t)
			msh := &mockSyspolicyHandler{
				t: t,
				stringPolicies: map[syspolicy.Key]*string{
					syspolicy.ExitNodeID: nil,
					syspolicy.ExitNodeIP: nil,
				},
			}
			if test.exitNodeIDKey {
				msh.stringPolicies[syspolicy.ExitNodeID] = &test.exitNodeID
			}
			if test.exitNodeIPKey {
				msh.stringPolicies[syspolicy.ExitNodeIP] = &test.exitNodeIP
			}
			syspolicy.SetHandlerForTest(t, msh)
			if test.nm == nil {
				test.nm = new(netmap.NetworkMap)
			}
			if test.prefs == nil {
				test.prefs = ipn.NewPrefs()
			}
			pm := must.Get(newProfileManager(new(mem.Store), t.Logf))
			pm.prefs = test.prefs.View()
			b.netMap = test.nm
			b.pm = pm
			changed := setExitNodeID(b.pm.prefs.AsStruct(), test.nm)
			b.SetPrefs(pm.CurrentPrefs().AsStruct())
			if got := b.pm.prefs.ExitNodeID(); got != tailcfg.StableNodeID(test.exitNodeIDWant) {
				t.Errorf("got %v want %v", got, test.exitNodeIDWant)
			}
			if got := b.pm.prefs.ExitNodeIP(); test.exitNodeIPWant == "" {
				if got.String() != "invalid IP" {
					t.Errorf("got %v want invalid IP", got)
				}
			} else if got.String() != test.exitNodeIPWant {
				t.Errorf("got %v want %v", got, test.exitNodeIPWant)
			}

			if changed != test.prefsChanged {
				t.Errorf("wanted prefs changed %v, got prefs changed %v", test.prefsChanged, changed)
			}
		})
	}
}

func TestApplySysPolicy(t *testing.T) {
	tests := []struct {
		name           string
		prefs          ipn.Prefs
		wantPrefs      ipn.Prefs
		wantAnyChange  bool
		stringPolicies map[syspolicy.Key]string
	}{
		{
			name: "empty prefs without policies",
		},
		{
			name: "prefs set without policies",
			prefs: ipn.Prefs{
				ControlURL:             "1",
				ShieldsUp:              true,
				ForceDaemon:            true,
				ExitNodeAllowLANAccess: true,
				CorpDNS:                true,
				RouteAll:               true,
			},
			wantPrefs: ipn.Prefs{
				ControlURL:             "1",
				ShieldsUp:              true,
				ForceDaemon:            true,
				ExitNodeAllowLANAccess: true,
				CorpDNS:                true,
				RouteAll:               true,
			},
		},
		{
			name: "empty prefs with policies",
			wantPrefs: ipn.Prefs{
				ControlURL:             "1",
				ShieldsUp:              true,
				ForceDaemon:            true,
				ExitNodeAllowLANAccess: true,
				CorpDNS:                true,
				RouteAll:               true,
			},
			wantAnyChange: true,
			stringPolicies: map[syspolicy.Key]string{
				syspolicy.ControlURL:                "1",
				syspolicy.EnableIncomingConnections: "never",
				syspolicy.EnableServerMode:          "always",
				syspolicy.ExitNodeAllowLANAccess:    "always",
				syspolicy.EnableTailscaleDNS:        "always",
				syspolicy.EnableTailscaleSubnets:    "always",
			},
		},
		{
			name: "prefs set with matching policies",
			prefs: ipn.Prefs{
				ControlURL:  "1",
				ShieldsUp:   true,
				ForceDaemon: true,
			},
			wantPrefs: ipn.Prefs{
				ControlURL:  "1",
				ShieldsUp:   true,
				ForceDaemon: true,
			},
			stringPolicies: map[syspolicy.Key]string{
				syspolicy.ControlURL:                "1",
				syspolicy.EnableIncomingConnections: "never",
				syspolicy.EnableServerMode:          "always",
				syspolicy.ExitNodeAllowLANAccess:    "never",
				syspolicy.EnableTailscaleDNS:        "never",
				syspolicy.EnableTailscaleSubnets:    "never",
			},
		},
		{
			name: "prefs set with conflicting policies",
			prefs: ipn.Prefs{
				ControlURL:             "1",
				ShieldsUp:              true,
				ForceDaemon:            true,
				ExitNodeAllowLANAccess: false,
				CorpDNS:                true,
				RouteAll:               false,
			},
			wantPrefs: ipn.Prefs{
				ControlURL:             "2",
				ShieldsUp:              false,
				ForceDaemon:            false,
				ExitNodeAllowLANAccess: true,
				CorpDNS:                false,
				RouteAll:               true,
			},
			wantAnyChange: true,
			stringPolicies: map[syspolicy.Key]string{
				syspolicy.ControlURL:                "2",
				syspolicy.EnableIncomingConnections: "always",
				syspolicy.EnableServerMode:          "never",
				syspolicy.ExitNodeAllowLANAccess:    "always",
				syspolicy.EnableTailscaleDNS:        "never",
				syspolicy.EnableTailscaleSubnets:    "always",
			},
		},
		{
			name: "prefs set with neutral policies",
			prefs: ipn.Prefs{
				ControlURL:             "1",
				ShieldsUp:              true,
				ForceDaemon:            true,
				ExitNodeAllowLANAccess: false,
				CorpDNS:                true,
				RouteAll:               true,
			},
			wantPrefs: ipn.Prefs{
				ControlURL:             "1",
				ShieldsUp:              true,
				ForceDaemon:            true,
				ExitNodeAllowLANAccess: false,
				CorpDNS:                true,
				RouteAll:               true,
			},
			stringPolicies: map[syspolicy.Key]string{
				syspolicy.EnableIncomingConnections: "user-decides",
				syspolicy.EnableServerMode:          "user-decides",
				syspolicy.ExitNodeAllowLANAccess:    "user-decides",
				syspolicy.EnableTailscaleDNS:        "user-decides",
				syspolicy.EnableTailscaleSubnets:    "user-decides",
			},
		},
		{
			name: "ControlURL",
			wantPrefs: ipn.Prefs{
				ControlURL: "set",
			},
			wantAnyChange: true,
			stringPolicies: map[syspolicy.Key]string{
				syspolicy.ControlURL: "set",
			},
		},
		{
			name: "enable AutoUpdate apply does not unset check",
			prefs: ipn.Prefs{
				AutoUpdate: ipn.AutoUpdatePrefs{
					Check: true,
					Apply: opt.NewBool(false),
				},
			},
			wantPrefs: ipn.Prefs{
				AutoUpdate: ipn.AutoUpdatePrefs{
					Check: true,
					Apply: opt.NewBool(true),
				},
			},
			wantAnyChange: true,
			stringPolicies: map[syspolicy.Key]string{
				syspolicy.ApplyUpdates: "always",
			},
		},
		{
			name: "disable AutoUpdate apply does not unset check",
			prefs: ipn.Prefs{
				AutoUpdate: ipn.AutoUpdatePrefs{
					Check: true,
					Apply: opt.NewBool(true),
				},
			},
			wantPrefs: ipn.Prefs{
				AutoUpdate: ipn.AutoUpdatePrefs{
					Check: true,
					Apply: opt.NewBool(false),
				},
			},
			wantAnyChange: true,
			stringPolicies: map[syspolicy.Key]string{
				syspolicy.ApplyUpdates: "never",
			},
		},
		{
			name: "enable AutoUpdate check does not unset apply",
			prefs: ipn.Prefs{
				AutoUpdate: ipn.AutoUpdatePrefs{
					Check: false,
					Apply: opt.NewBool(true),
				},
			},
			wantPrefs: ipn.Prefs{
				AutoUpdate: ipn.AutoUpdatePrefs{
					Check: true,
					Apply: opt.NewBool(true),
				},
			},
			wantAnyChange: true,
			stringPolicies: map[syspolicy.Key]string{
				syspolicy.CheckUpdates: "always",
			},
		},
		{
			name: "disable AutoUpdate check does not unset apply",
			prefs: ipn.Prefs{
				AutoUpdate: ipn.AutoUpdatePrefs{
					Check: true,
					Apply: opt.NewBool(true),
				},
			},
			wantPrefs: ipn.Prefs{
				AutoUpdate: ipn.AutoUpdatePrefs{
					Check: false,
					Apply: opt.NewBool(true),
				},
			},
			wantAnyChange: true,
			stringPolicies: map[syspolicy.Key]string{
				syspolicy.CheckUpdates: "never",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msh := &mockSyspolicyHandler{
				t:              t,
				stringPolicies: make(map[syspolicy.Key]*string, len(tt.stringPolicies)),
			}
			for p, v := range tt.stringPolicies {
				v := v // construct a unique pointer for each policy value
				msh.stringPolicies[p] = &v
			}
			syspolicy.SetHandlerForTest(t, msh)

			t.Run("unit", func(t *testing.T) {
				prefs := tt.prefs.Clone()

				gotAnyChange := applySysPolicy(prefs)

				if gotAnyChange && prefs.Equals(&tt.prefs) {
					t.Errorf("anyChange but prefs is unchanged: %v", prefs.Pretty())
				}
				if !gotAnyChange && !prefs.Equals(&tt.prefs) {
					t.Errorf("!anyChange but prefs changed from %v to %v", tt.prefs.Pretty(), prefs.Pretty())
				}
				if gotAnyChange != tt.wantAnyChange {
					t.Errorf("anyChange=%v, want %v", gotAnyChange, tt.wantAnyChange)
				}
				if !prefs.Equals(&tt.wantPrefs) {
					t.Errorf("prefs=%v, want %v", prefs.Pretty(), tt.wantPrefs.Pretty())
				}
			})

			t.Run("set prefs", func(t *testing.T) {
				b := newTestBackend(t)
				b.SetPrefs(tt.prefs.Clone())
				if !b.Prefs().Equals(tt.wantPrefs.View()) {
					t.Errorf("prefs=%v, want %v", b.Prefs().Pretty(), tt.wantPrefs.Pretty())
				}
			})

			t.Run("status update", func(t *testing.T) {
				// Profile manager fills in blank ControlURL but it's not set
				// in most test cases to avoid cluttering them, so adjust for
				// that.
				usePrefs := tt.prefs.Clone()
				if usePrefs.ControlURL == "" {
					usePrefs.ControlURL = ipn.DefaultControlURL
				}
				wantPrefs := tt.wantPrefs.Clone()
				if wantPrefs.ControlURL == "" {
					wantPrefs.ControlURL = ipn.DefaultControlURL
				}

				pm := must.Get(newProfileManager(new(mem.Store), t.Logf))
				pm.prefs = usePrefs.View()

				b := newTestBackend(t)
				b.mu.Lock()
				b.pm = pm
				b.mu.Unlock()

				b.SetControlClientStatus(b.cc, controlclient.Status{})
				if !b.Prefs().Equals(wantPrefs.View()) {
					t.Errorf("prefs=%v, want %v", b.Prefs().Pretty(), wantPrefs.Pretty())
				}
			})
		})
	}
}

func TestPreferencePolicyInfo(t *testing.T) {
	tests := []struct {
		name         string
		initialValue bool
		wantValue    bool
		wantChange   bool
		policyValue  string
		policyError  error
	}{
		{
			name:         "force enable modify",
			initialValue: false,
			wantValue:    true,
			wantChange:   true,
			policyValue:  "always",
		},
		{
			name:         "force enable unchanged",
			initialValue: true,
			wantValue:    true,
			policyValue:  "always",
		},
		{
			name:         "force disable modify",
			initialValue: true,
			wantValue:    false,
			wantChange:   true,
			policyValue:  "never",
		},
		{
			name:         "force disable unchanged",
			initialValue: false,
			wantValue:    false,
			policyValue:  "never",
		},
		{
			name:         "unforced enabled",
			initialValue: true,
			wantValue:    true,
			policyValue:  "user-decides",
		},
		{
			name:         "unforced disabled",
			initialValue: false,
			wantValue:    false,
			policyValue:  "user-decides",
		},
		{
			name:         "blank enabled",
			initialValue: true,
			wantValue:    true,
			policyValue:  "",
		},
		{
			name:         "blank disabled",
			initialValue: false,
			wantValue:    false,
			policyValue:  "",
		},
		{
			name:         "unset enabled",
			initialValue: true,
			wantValue:    true,
			policyError:  syspolicy.ErrNoSuchKey,
		},
		{
			name:         "unset disabled",
			initialValue: false,
			wantValue:    false,
			policyError:  syspolicy.ErrNoSuchKey,
		},
		{
			name:         "error enabled",
			initialValue: true,
			wantValue:    true,
			policyError:  errors.New("test error"),
		},
		{
			name:         "error disabled",
			initialValue: false,
			wantValue:    false,
			policyError:  errors.New("test error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, pp := range preferencePolicies {
				t.Run(string(pp.key), func(t *testing.T) {
					var h syspolicy.Handler

					allPolicies := make(map[syspolicy.Key]*string, len(preferencePolicies)+1)
					allPolicies[syspolicy.ControlURL] = nil
					for _, pp := range preferencePolicies {
						allPolicies[pp.key] = nil
					}

					if tt.policyError != nil {
						h = &errorSyspolicyHandler{
							t:         t,
							err:       tt.policyError,
							key:       pp.key,
							allowKeys: allPolicies,
						}
					} else {
						msh := &mockSyspolicyHandler{
							t:                   t,
							stringPolicies:      allPolicies,
							failUnknownPolicies: true,
						}
						msh.stringPolicies[pp.key] = &tt.policyValue
						h = msh
					}
					syspolicy.SetHandlerForTest(t, h)

					prefs := defaultPrefs.AsStruct()
					pp.set(prefs, tt.initialValue)

					gotAnyChange := applySysPolicy(prefs)

					if gotAnyChange != tt.wantChange {
						t.Errorf("anyChange=%v, want %v", gotAnyChange, tt.wantChange)
					}
					got := pp.get(prefs.View())
					if got != tt.wantValue {
						t.Errorf("pref=%v, want %v", got, tt.wantValue)
					}
				})
			}
		})
	}
}

func TestOnTailnetDefaultAutoUpdate(t *testing.T) {
	tests := []struct {
		desc           string
		before, after  opt.Bool
		tailnetDefault bool
	}{
		{
			before:         opt.Bool(""),
			tailnetDefault: true,
			after:          opt.NewBool(true),
		},
		{
			before:         opt.Bool(""),
			tailnetDefault: false,
			after:          opt.NewBool(false),
		},
		{
			before:         opt.Bool("unset"),
			tailnetDefault: true,
			after:          opt.NewBool(true),
		},
		{
			before:         opt.Bool("unset"),
			tailnetDefault: false,
			after:          opt.NewBool(false),
		},
		{
			before:         opt.NewBool(false),
			tailnetDefault: true,
			after:          opt.NewBool(false),
		},
		{
			before:         opt.NewBool(true),
			tailnetDefault: false,
			after:          opt.NewBool(true),
		},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("before=%s after=%s", tt.before, tt.after), func(t *testing.T) {
			b := newTestBackend(t)
			p := ipn.NewPrefs()
			p.AutoUpdate.Apply = tt.before
			if err := b.pm.setPrefsLocked(p.View()); err != nil {
				t.Fatal(err)
			}
			b.onTailnetDefaultAutoUpdate(tt.tailnetDefault)
			if want, got := tt.after, b.pm.CurrentPrefs().AutoUpdate().Apply; got != want {
				t.Errorf("got: %q, want %q", got, want)
			}
		})
	}
}
