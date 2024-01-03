// Package portmap implements port mapping using NAT-PMP or uPNP.
package portmap

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sync"
	"time"

	ig1 "github.com/huin/goupnp/dcps/internetgateway1"
	ig2 "github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/jackpal/gateway"
	natpmp "github.com/jackpal/go-nat-pmp"
)

const (
	NATPMP = 1
	UPNP   = 2
	All    = NATPMP | UPNP
)

type portmapClient interface {
	addPortMapping(ctx context.Context, label string, protocol string, port, externalPort uint16, lifetime int) (uint16, int, error)
}

// natpmpClient implements portmapClient for NAT-PMP.
type natpmpClient natpmp.Client

func wait(ctx context.Context, ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newNatpmpClient attempts to contact the NAT-PMP client on the default
// gateway and returns a natpmpClient structure if successful.
func newNatpmpClient(ctx context.Context) (*natpmpClient, error) {
	g, err := gateway.DiscoverGateway()
	if err != nil {
		return nil, err
	}

	c := natpmp.NewClient(g)

	// NewClient always succeeds, verify that the gateway actually
	// supports NAT-PMP.
	ch := make(chan error, 1)
	go func() {
		defer close(ch)
		_, err = c.GetExternalAddress()
		if err != nil {
			ch <- err
		}
	}()
	err = wait(ctx, ch)
	if err != nil {
		return nil, err
	}
	return (*natpmpClient)(c), nil
}

// AddPortMapping maps a port for the given lifetime.  It returns the
// allocated external port, which might be different from the port
// requested, and a lifetime in seconds.
func (c *natpmpClient) addPortMapping(ctx context.Context, label string, protocol string, port, externalPort uint16, lifetime int) (uint16, int, error) {
	var r *natpmp.AddPortMappingResult
	ch := make(chan error, 1)
	go func() {
		defer close(ch)
		var err error
		r, err = (*natpmp.Client)(c).AddPortMapping(
			protocol, int(port), int(externalPort), lifetime,
		)
		if err != nil {
			ch <- err
		}
	}()
	err := wait(ctx, ch)
	if err != nil {
		return 0, 0, err
	}
	return r.MappedExternalPort, int(r.PortMappingLifetimeInSeconds), nil
}

type WANIPConnection interface {
	GetSpecificPortMappingEntryCtx(
		context.Context, string, uint16, string,
	) (uint16, string, bool, string, uint32, error)
	AddPortMappingCtx(
		context.Context, string, uint16, string, uint16, string,
		bool, string, uint32,
	) error
	DeletePortMappingCtx(context.Context, string, uint16, string) error
}

func getWANIPLocation(client WANIPConnection) *url.URL {
	switch client := client.(type) {
	case *ig1.WANIPConnection1:
		return client.Location
	case *ig2.WANIPConnection1:
		return client.Location
	case *ig2.WANIPConnection2:
		return client.Location
	default:
		return nil
	}
}

// upnpClient implements portmapClient for uPNP.
type upnpClient struct {
	client WANIPConnection
}

// newUpnpClient attempts to discover a WAN IP Connection client on the
// local network.  If more than one are found, it tries to return one
// that is on the default route.
func newUpnpClient(ctx context.Context) (*upnpClient, error) {
	clients2, _, err := ig2.NewWANIPConnection2ClientsCtx(ctx)
	if err != nil {
		clients2 = nil
	}
	var clients1 []*ig1.WANIPConnection1
	if len(clients2) == 0 {
		clients1, _, err = ig1.NewWANIPConnection1ClientsCtx(ctx)
		if err != nil {
			return nil, err
		}
	}

	var clients []WANIPConnection
	for _, c2 := range clients2 {
		clients = append(clients, c2)
	}
	for _, c1 := range clients1 {
		clients = append(clients, c1)
	}

	if len(clients) == 0 {
		return nil, errors.New("no UPNP gateways found")
	}
	if len(clients) == 1 {
		return &upnpClient{client: clients[0]}, nil
	}

	gw, err := gateway.DiscoverGateway()
	if err != nil {
		return &upnpClient{client: clients[0]}, nil
	}

	for _, client := range clients {
		location := getWANIPLocation(client)
		if location == nil {
			continue
		}
		host, _, err := net.SplitHostPort(location.Host)
		if err != nil {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil {
			continue
		}
		if ip.Equal(gw) {
			return &upnpClient{client: client}, nil
		}
	}

	return &upnpClient{client: clients[0]}, nil
}

// getMyIPv4 returns the local IPv4 address used by the default route.
func getMyIPv4() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, errors.New("unexpected type for local address")
	}

	return localAddr.IP, nil
}

// addPortMapping attempts to create a mapping for the given port.  If
// successful, it returns the allocated external port, which might be
// different from the requested port if the latter was alredy allocated by
// a different host, and a lifetime in seconds.
func (c *upnpClient) addPortMapping(ctx context.Context, label string, protocol string, port, externalPort uint16, lifetime int) (uint16, int, error) {
	var prot string
	switch protocol {
	case "tcp":
		prot = "TCP"
	case "udp":
		prot = "UDP"
	default:
		return 0, 0, errors.New("unknown protocol")
	}

	myip, err := getMyIPv4()
	if err != nil {
		return 0, 0, err
	}

	ipc := c.client

	// Find a free port
	ep := externalPort
	ok := false
	for ep < 65535 {
		p, c, e, _, l, err :=
			ipc.GetSpecificPortMappingEntryCtx(ctx, "", ep, prot)
		if err != nil || e == false || l <= 0 {
			ok = true
			break
		}
		a := net.ParseIP(c)
		if a.Equal(myip) && p == port {
			ok = true
			break
		}
		if lifetime == 0 {
			return 0, 0, errors.New("mapping not found")
		}
		ep++
	}

	if !ok {
		return 0, 0, errors.New("couldn't find free port")
	}

	if lifetime > 0 {
		err = ipc.AddPortMappingCtx(
			ctx, "", ep, prot,
			port, myip.String(), true,
			label, uint32(lifetime),
		)
		if err != nil {
			return 0, 0, err
		}
		return ep, lifetime, nil
	}

	err = ipc.DeletePortMappingCtx(ctx, "", ep, prot)
	if err != nil {
		return 0, 0, err
	}
	return ep, 0, nil
}

// newClient attempts to contact a NAT-PMP gateway; if that fails, it
// attempts to discover a uPNP gateway.
func newClient(ctx context.Context, kind int) (portmapClient, error) {
	var err error
	if (kind & NATPMP) != 0 {
		c, err1 := newNatpmpClient(ctx)
		if err1 == nil {
			return c, nil
		}
		err = err1
	}

	if (kind & UPNP) != 0 {
		c, err1 := newUpnpClient(ctx)
		if err1 == nil {
			return c, nil
		}
		if err == nil {
			err = err1
		} else {
			err = errors.New(err.Error() + " and " + err1.Error())
		}
	}

	if err == nil {
		err = errors.New("no portmapping protocol found")
	}

	return nil, err
}

// clientCache is a thread-safe cache for a portmapping client
type clientCache struct {
	mu     sync.Mutex
	client portmapClient
}

// get fetches the client stored in the cache, or creates a new one
func (cache *clientCache) get(ctx context.Context, kind int) (portmapClient, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if cache.client != nil {
		return cache.client, nil
	}

	c, err := newClient(ctx, kind)
	if err != nil {
		return nil, err
	}

	cache.client = c
	return c, nil
}

// reset resets the cache
func (cache *clientCache) reset() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.client = nil
}

// Status is passed to the callback of Map
type Status struct {
	Internal, External uint16
	Lifetime           time.Duration
}

// Map runs a portmapping loop for both TCP and UDP.  The kind parameter
// indicates the portmapping protocols to attempt.
//
// The label is passed to the UPNP server (it is not used with NAT-PMP)
// and may be displayed in the router's user interface.
//
// The callback function is called whenever a mapping is established or
// changes, or when an error occurs; it may be nil.
func Map(ctx context.Context, label string, internal uint16, kind int, f func(proto string, status Status, err error)) error {
	cache := &clientCache{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		domap(ctx, label, cache, "tcp", internal, kind, f)
		wg.Done()
	}()
	go func() {
		domap(ctx, label, cache, "udp", internal, kind, f)
		wg.Done()
	}()
	wg.Wait()
	return nil
}

func domap(ctx context.Context, label string, cache *clientCache, proto string, internal uint16, kind int, f func(proto string, status Status, err error)) {
	var client portmapClient
	external := internal
	unmap := func(err error) {
		status := Status{
			Internal: internal,
			External: 0,
			Lifetime: 0,
		}
		if client != nil {
			ctx2, cancel := context.WithTimeout(
				context.Background(), 2*time.Second,
			)
			_, _, err2 := client.addPortMapping(
				ctx2, label, proto, internal, external, 0,
			)
			cancel()
			if err == nil {
				err = err2
			}
			client = nil
			if f != nil {
				f(proto, status, err)
			}
		} else if err != nil {
			if f != nil {
				f(proto, status, err)
			}

		}
	}

	defer unmap(nil)

	sleep := func(d time.Duration) error {
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}

	for {
		c, err := cache.get(ctx, kind)
		if err != nil {
			unmap(err)
			err = sleep(30 * time.Second)
			if err != nil {
				return
			}
			continue
		}
		if c != client {
			unmap(nil)
			client = c
		}

		ep, lifetime, err := client.addPortMapping(
			ctx, label, proto, internal, external, 30*60,
		)
		if err != nil {
			unmap(err)
			cache.reset()
			client = nil
			err = sleep(5 * time.Second)
			if err != nil {
				return
			}
			continue
		}
		external = ep
		if lifetime < 30 {
			lifetime = 30
		}
		if f != nil {
			f(proto, Status{
				Internal: internal,
				External: external,
				Lifetime: time.Duration(lifetime) * time.Second,
			}, nil)
		}
		err = sleep(time.Duration(lifetime) * time.Second * 2 / 3)
		if err != nil {
			return
		}
	}
}
