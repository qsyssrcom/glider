package strategy

import (
	"bytes"
	"hash/fnv"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nadoo/glider/common/log"
	"github.com/nadoo/glider/proxy"
)

// Config is strategy config struct.
type Config struct {
	Strategy      string
	CheckWebSite  string
	CheckInterval int
	CheckTimeout  int
	MaxFailures   int
	IntFace       string
}

// forwarder slice orderd by priority
type priSlice []*Forwarder

func (p priSlice) Len() int           { return len(p) }
func (p priSlice) Less(i, j int) bool { return p[i].Priority() > p[j].Priority() }
func (p priSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// Proxy is base proxy struct.
type Proxy struct {
	config   *Config
	fwdrs    priSlice
	avail    []*Forwarder // available forwarders
	mu       sync.RWMutex
	index    uint32
	priority uint32
	next     func(addr string) *Forwarder
}

// NewProxy returns a new strategy proxy.
func NewProxy(s []string, c *Config) *Proxy {
	var fwdrs []*Forwarder
	for _, chain := range s {
		fwdr, err := ForwarderFromURL(chain, c.IntFace)
		if err != nil {
			log.Fatal(err)
		}
		fwdr.SetMaxFailures(uint32(c.MaxFailures))
		fwdrs = append(fwdrs, fwdr)
	}

	if len(fwdrs) == 0 {
		// direct forwarder
		fwdrs = append(fwdrs, DirectForwarder(c.IntFace))
		c.Strategy = "rr"
	}

	return newProxy(fwdrs, c)
}

// newProxy returns a new rrProxy
func newProxy(fwdrs []*Forwarder, c *Config) *Proxy {
	p := &Proxy{fwdrs: fwdrs, config: c}
	sort.Sort(p.fwdrs)

	p.init()

	if strings.IndexByte(p.config.CheckWebSite, ':') == -1 {
		p.config.CheckWebSite += ":80"
	}

	switch c.Strategy {
	case "rr":
		p.next = p.scheduleRR
		log.F("[strategy] forward to remote servers in round robin mode.")
	case "ha":
		p.next = p.scheduleHA
		log.F("[strategy] forward to remote servers in high availability mode.")
	case "lha":
		p.next = p.scheduleLHA
		log.F("[strategy] forward to remote servers in latency based high availability mode.")
	case "dh":
		p.next = p.scheduleDH
		log.F("[strategy] forward to remote servers in destination hashing mode.")
	default:
		p.next = p.scheduleRR
		log.F("[strategy] not supported forward mode '%s', use round robin mode.", c.Strategy)
	}

	for _, f := range fwdrs {
		f.AddHandler(p.onStatusChanged)
	}

	return p
}

// Dial connects to the address addr on the network net.
func (p *Proxy) Dial(network, addr string) (net.Conn, string, error) {
	nd := p.NextDialer(addr)
	c, err := nd.Dial(network, addr)
	return c, nd.Addr(), err
}

// DialUDP connects to the given address.
func (p *Proxy) DialUDP(network, addr string) (pc net.PacketConn, writeTo net.Addr, err error) {
	return p.NextDialer(addr).DialUDP(network, addr)
}

// NextDialer returns the next dialer.
func (p *Proxy) NextDialer(dstAddr string) proxy.Dialer {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.avail) == 0 {
		return p.fwdrs[atomic.AddUint32(&p.index, 1)%uint32(len(p.fwdrs))]
	}

	return p.next(dstAddr)
}

// Priority returns the active priority of dialer.
func (p *Proxy) Priority() uint32 { return atomic.LoadUint32(&p.priority) }

// SetPriority sets the active priority of daler.
func (p *Proxy) SetPriority(pri uint32) { atomic.StoreUint32(&p.priority, pri) }

// init traverse d.fwdrs and init the available forwarder slice.
func (p *Proxy) init() {
	for _, f := range p.fwdrs {
		if f.Enabled() {
			p.SetPriority(f.Priority())
			break
		}
	}

	p.avail = nil
	for _, f := range p.fwdrs {
		if f.Enabled() && f.Priority() >= p.Priority() {
			p.avail = append(p.avail, f)
		}
	}

	if len(p.avail) == 0 {
		// no available forwarders, set priority to 0 to check all forwarders in check func
		p.SetPriority(0)
		log.F("[strategy] no available forwarders, please check your config file or network settings")
	}
}

// onStatusChanged will be called when fwdr's status changed.
func (p *Proxy) onStatusChanged(fwdr *Forwarder) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if fwdr.Enabled() {
		log.F("[strategy] %s changed status from Disabled to Enabled ", fwdr.Addr())
		if fwdr.Priority() == p.Priority() {
			p.avail = append(p.avail, fwdr)
		} else if fwdr.Priority() > p.Priority() {
			p.init()
		}
	} else {
		log.F("[strategy] %s changed status from Enabled to Disabled", fwdr.Addr())
		for i, f := range p.avail {
			if f == fwdr {
				p.avail[i], p.avail = p.avail[len(p.avail)-1], p.avail[:len(p.avail)-1]
				break
			}
		}
	}

	if len(p.avail) == 0 {
		p.init()
	}
}

// Check implements the Checker interface.
func (p *Proxy) Check() {
	// no need to check when there's only 1 forwarder
	if len(p.fwdrs) > 1 {
		for i := 0; i < len(p.fwdrs); i++ {
			go p.check(i)
		}
	}
}

func (p *Proxy) check(i int) {
	f := p.fwdrs[i]
	retry := 1
	buf := make([]byte, 4)

	for {
		time.Sleep(time.Duration(p.config.CheckInterval) * time.Second * time.Duration(retry>>1))

		// check all forwarders at least one time
		if retry > 1 && f.Priority() < p.Priority() {
			continue
		}

		retry <<= 1
		if retry > 16 {
			retry = 16
		}

		startTime := time.Now()
		rc, err := f.Dial("tcp", p.config.CheckWebSite)
		if err != nil {
			f.Disable()
			log.F("[check] %s(%d) -> %s, DISABLED. error in dial: %s", f.Addr(), f.Priority(), p.config.CheckWebSite, err)
			continue
		}

		rc.Write([]byte("GET / HTTP/1.0\r\n\r\n"))

		_, err = io.ReadFull(rc, buf)
		if err != nil {
			f.Disable()
			log.F("[check] %s(%d) -> %s, DISABLED. error in read: %s", f.Addr(), f.Priority(), p.config.CheckWebSite, err)
		} else if bytes.Equal([]byte("HTTP"), buf) {

			readTime := time.Since(startTime)
			f.SetLatency(int64(readTime))

			if readTime > time.Duration(p.config.CheckTimeout)*time.Second {
				f.Disable()
				log.F("[check] %s(%d) -> %s, DISABLED. check timeout: %s", f.Addr(), f.Priority(), p.config.CheckWebSite, readTime)
			} else {
				retry = 2
				f.Enable()
				log.F("[check] %s(%d) -> %s, ENABLED. connect time: %s", f.Addr(), f.Priority(), p.config.CheckWebSite, readTime)
			}

		} else {
			f.Disable()
			log.F("[check] %s(%d) -> %s, DISABLED. server response: %s", f.Addr(), f.Priority(), p.config.CheckWebSite, buf)
		}

		rc.Close()
	}
}

// Round Robin
func (p *Proxy) scheduleRR(dstAddr string) *Forwarder {
	return p.avail[atomic.AddUint32(&p.index, 1)%uint32(len(p.avail))]
}

// High Availability
func (p *Proxy) scheduleHA(dstAddr string) *Forwarder {
	return p.avail[0]
}

// Latency based High Availability
func (p *Proxy) scheduleLHA(dstAddr string) *Forwarder {
	fwdr := p.avail[0]
	lowest := fwdr.Latency()
	for _, f := range p.avail {
		if f.Latency() < lowest {
			lowest = f.Latency()
			fwdr = f
		}
	}
	return fwdr
}

// Destination Hashing
func (p *Proxy) scheduleDH(dstAddr string) *Forwarder {
	fnv1a := fnv.New32a()
	fnv1a.Write([]byte(dstAddr))
	return p.avail[fnv1a.Sum32()%uint32(len(p.avail))]
}
