package netconf

import (
	"encoding/xml"
	"fmt"

	"github.com/satori/go.uuid"

	"io"
	"log"
	"sync"

	"github.com/damianoneill/net/netconf/rfc6242"
)

// The Operations layer defines a set of base protocol operations
// invoked as RPC methods with XML-encoded parameters.

// Session represents a Netconf Session
type Session interface {
	// Execute executes an RPC request on the server and returns the reply.
	Execute(req Request) (*RPCReply, error)

	// ExecuteAsync submits an RPC request for execution on the server, arranging for the
	// reply to be sent to the supplied channel.
	ExecuteAsync(req Request, rchan chan *RPCReply) (err error)

	// Subscribe issues an RPC request and returns the reply. If successful, notifications will
	// be sent to the supplied channel.
	Subscribe(req Request, nchan chan *Notification) (reply *RPCReply, err error)

	// Close closes the session and releases any associated resources.
	Close()
}

type sesImpl struct {
	t      Transport
	dec    *decoder
	enc    *encoder
	nclog  *log.Logger
	evtlog *log.Logger

	pool []chan *RPCReply

	hellochan chan *HelloMessage
	responseq []chan *RPCReply
	subchan   chan *Notification

	hello   *HelloMessage
	reqLock sync.Mutex
	pchLock sync.Mutex
	rchLock sync.Mutex
}

// DefaultCapabilities sets the default capabilities of the client library
var DefaultCapabilities = []string{
	CapBase10,
}

var (
	netconfNS       = "urn:ietf:params:xml:ns:netconf:base:1.0"
	netconfNotifyNS = "urn:ietf:params:xml:ns:netconf:notification:1.0"
	nameHello       = xml.Name{Space: netconfNS, Local: "hello"}
	nameRPCReply    = xml.Name{Space: netconfNS, Local: "rpc-reply"}
	notification    = xml.Name{Space: netconfNotifyNS, Local: "notification"}
	CapBase10       = "urn:ietf:params:netconf:base:1.0"
	CapBase11       = "urn:ietf:params:netconf:base:1.1"
)

// NewSession creates a new Netconf session, using the supplied Transport.
func NewSession(t Transport, evtlog *log.Logger, nclog *log.Logger) (Session, error) {

	dec := newDecoder(t)
	enc := newEncoder(t)

	sess := &sesImpl{t: t, dec: dec, enc: enc, evtlog: evtlog, nclog: nclog, hellochan: make(chan *HelloMessage)}

	go sess.handleInput()

	sess.hello = <-sess.hellochan

	helloresp := &HelloMessage{Capabilities: DefaultCapabilities}
	chunkedFraming := false
	for _, capability := range sess.hello.Capabilities {
		if capability == CapBase11 {
			helloresp.Capabilities = []string{CapBase11}
			chunkedFraming = true
			break
		}
	}

	err := sess.enc.encode(helloresp)
	if err != nil {
		return nil, err
	}

	if chunkedFraming {
		rfc6242.SetChunkedFraming(sess.dec.ncDecoder, sess.enc.ncEncoder)
	}

	return sess, nil
}

func (si *sesImpl) Execute(req Request) (*RPCReply, error) {

	rchan := si.allocChan()
	defer si.relChan(rchan)

	err := si.ExecuteAsync(req, rchan)
	if err != nil {
		return nil, err
	}
	reply := <-rchan
	return reply, nil
}

func (si *sesImpl) ExecuteAsync(req Request, rchan chan *RPCReply) (err error) {
	si.reqLock.Lock()
	defer si.reqLock.Unlock()
	msg := &RPCMessage{MessageID: uuid.NewV4().String(), Methods: []byte(string(req))}

	si.pushRespChan(rchan)

	return si.enc.encode(msg)
}

func (si *sesImpl) Subscribe(req Request, nchan chan *Notification) (reply *RPCReply, err error) {
	rchan := si.allocChan()
	defer si.relChan(rchan)

	err = si.ExecuteAsync(req, rchan)
	if err != nil {
		return
	}
	si.subchan = nchan
	reply = <-rchan
	return
}

func (si *sesImpl) Close() {
	err := si.t.Close()
	if err != nil {
		si.evtlog.Printf("Session close failed %v\n", err)
	}
}

func (si *sesImpl) handleInput() {

	defer si.closeChannels()
	for {
		token, err := si.dec.Token()
		if err != nil {
			if err != io.EOF {
				si.evtlog.Printf("Token() error: %v\n", err)
			}
			break
		}
		switch token := token.(type) {
		case xml.StartElement:
			switch token.Name {
			case nameHello: // <hello>
				hello := HelloMessage{}
				if err := si.dec.DecodeElement(&hello, &token); err != nil {
					si.evtlog.Printf("DecodeElement() error: %v\n", err)
					return
				}
				si.hellochan <- &hello
			case nameRPCReply: // <rpc-reply>
				reply := RPCReply{}
				if err := si.dec.DecodeElement(&reply, &token); err != nil {
					si.evtlog.Printf("DecodeElement() error: %v\n", err)
					return
				}

				respch := si.popRespChan()
				go func(ch chan *RPCReply, r *RPCReply) {
					ch <- r
				}(respch, &reply)

			case notification: // <notification>

				result := &NotificationMessage{}
				_ = si.dec.DecodeElement(result, &token)
				n := fmt.Sprintf(`<%s xmlns="%s">%s</%s>`,
					result.Event.XMLName.Local, result.Event.XMLName.Space, result.Event.Event, result.Event.XMLName.Local)
				if si.subchan != nil {
					si.subchan <- &Notification{XMLName: result.Event.XMLName, EventTime: result.EventTime, Event: n}
				}

			default:
				fmt.Printf("Unexpected element:%v\n", token.Name)

			}
		}
	}
}

func (si *sesImpl) closeChannels() {
	close(si.hellochan)
	if si.subchan != nil {
		close(si.subchan)
	}
	si.closeAllResponseChannels()
}

func (si *sesImpl) allocChan() (ch chan *RPCReply) {
	si.pchLock.Lock()
	defer si.pchLock.Unlock()

	l := len(si.pool)
	if l == 0 {
		return make(chan *RPCReply)
	}

	si.pool, ch = si.pool[:l-1], si.pool[l-1]
	return
}

func (si *sesImpl) relChan(ch chan *RPCReply) {
	si.pchLock.Lock()
	defer si.pchLock.Unlock()
	si.pool = append(si.pool, ch)
}

func (si *sesImpl) pushRespChan(ch chan *RPCReply) {
	si.rchLock.Lock()
	defer si.rchLock.Unlock()
	si.responseq = append(si.responseq, ch)

}

func (si *sesImpl) popRespChan() (ch chan *RPCReply) {
	si.rchLock.Lock()
	defer si.rchLock.Unlock()
	if len(si.responseq) > 0 {
		si.responseq, ch = si.responseq[1:], si.responseq[0]
	}
	return
}

func (si *sesImpl) closeAllResponseChannels() {
	for {
		if ch := si.popRespChan(); ch != nil {
			close(ch)
		} else {
			return
		}
	}
}
