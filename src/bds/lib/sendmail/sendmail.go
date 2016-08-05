package sendmail

import (
	log "github.com/Sirupsen/logrus"
	"net/smtp"
	"net"
	"sync"
)


// mail bounce handler
// paramters are (recipiant email address, from email address, network related error or nil for regular bounce)
type Bouncer func(string, string, error)

type connection struct {
	cl *smtp.Client
	access sync.RWMutex
}


// safely visit a conection with a function
func (c *connection) visit(visitor func(*smtp.Client) error) (err error) {
	c.access.Lock()
	err = visitor(c.cl)
	c.access.Unlock()
	return
}

// mail delivery
type Mailer struct {
	// a dial function to obtain outbound smtp client
	Dial Dialer
	// number of times to try to deliver mail
	// 0 for unlimited
	Retries int
	// called if mail bounces or fails to deliver
	Bounce Bouncer
	// domain resolver function
	Resolve Resolver
	// delivery success hook, called with (recipiant email address, from email address)
	Success func(string, string)
	// for pipelining
	conns map[string]*connection
	// mutex for conns
	cmtx sync.RWMutex
	
}

// create a new pooled mailer
func NewMailer() *Mailer {
	return &Mailer{
		conns: make(map[string] *connection),
	}
}

// delete connection from pool
func (s *Mailer) delConn(addr string) {
	s.cmtx.Lock()
	// safe delete
	_, ok := s.conns[addr]
	if ok {
		delete(s.conns, addr)
	}
	s.cmtx.Unlock()
}

// visit a pooled connection in a safe way
func (s *Mailer) visitConn(n, addr, localname string, dialer Dialer, visitor func(*smtp.Client) error) (err error) {
	var c *connection
	c, err = s.getConn(n, addr, localname, dialer)
	if err == nil {
		err = c.visit(visitor)
		if err != nil {
			s.delConn(addr)
		}
	}
	return
}

// get connection from pool, create if not there
func (s *Mailer) getConn(n, addr, localname string, dial Dialer) (sc *connection, err error) {
	s.cmtx.Lock()
	var ok bool
	sc, ok = s.conns[addr]
	s.cmtx.Unlock()
	if !ok {
		var c net.Conn
		c, err = dial(n, addr)
		if err == nil {
			// new connection
			var cl *smtp.Client
			cl, err = smtp.NewClient(c, addr)
			if err != nil {
				return
			}
			err = cl.Hello(localname)
			if err != nil {
				cl.Quit()
				return
			}
			sc = &connection{
				cl: cl,
			}
			// success
			s.cmtx.Lock()
			s.conns[addr] = sc
			s.cmtx.Unlock()
		}
	}
	return
}

// try delivering mail
// returns a DeliveryJob that can be cancelled
func (s *Mailer) Deliver(recip, from string, body []byte) (d *DeliverJob) {
	log.Infof("Delivering %d of mail to %s from %s", len(body), recip, from)
	dialer := s.Dial
	if dialer == nil {
		dialer = net.Dial
	}

	bounce := s.Bounce
	
	resolver := s.Resolve

	if resolver == nil {
		resolver = func(name string) (a net.Addr, err error) {
			log.Debugf("mx lookup for %s", name)
			var mx []*net.MX
			mx, err = net.LookupMX(name)
			if err != nil && mx != nil {
				for _, m := range mx {
					var ips []net.IP
					log.Debugf("resolve mx record %s", m.Host)
					ips, err = net.LookupIP(m.Host)
					if err == nil {
						for _, ip := range ips {
							a, err = net.ResolveIPAddr("ip", ip.String())
							if err == nil {
								log.Debugf("resolved %s to %s", name, a)
								return
							}
						}
					}
				}
			}
			return
		}
	}
	
	d = &DeliverJob{
		unlimited: s.Retries == 0,
		cancel: false,
		retries: s.Retries,
		visit: func(f func(*smtp.Client) error) error {
			r_addr := extractAddr(recip)
			a, err := resolver(r_addr)
			if err == nil {
				err = s.visitConn(a.Network(), r_addr, extractAddr(from), dialer, f)
			} else {
				log.Warnf("failed to resolve %s: %s", r_addr, err.Error())
			}
			return err
		},
		bounce: bounce,
		recip: recip,
		from: from,
		body: make([]byte, len(body)),
		Result: make(chan bool),
		delivered: s.Success,
	}
	// copy body into deliverer
	copy(d.body, body)
	// run delivery in background
	go d.run()
	return
}

// gracefully quit all polled connections and close down
func (m *Mailer) Quit() {
	log.Info("shutting down pooled mailer")
	m.cmtx.Lock()
	// collect connections
	var conns []*connection
	for _, c := range m.conns {
		conns = append(conns, c)
	}
	// close all connections
	for _, c := range conns {
		c.visit(func(cl *smtp.Client) error {
			return cl.Quit()
		})
	}
	// collect all names
	var names []string
	for n, _ := range m.conns {
		names = append(names, n)
	}
	// delete all connections
	for _, n := range names {
		delete(m.conns, n)
	}
	m.cmtx.Unlock()
}

