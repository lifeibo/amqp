// Copyright (c) 2012, Sean Treadway, SoundCloud Ltd.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// Source code and contact info at http://github.com/streadway/amqp

package amqp

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultHeartbeat = 10 * time.Second
const defaultConnectionTimeout = 30 * time.Second

const (
	readWriteTimeout         = time.Second * 30
)

type TimeoutConn struct {
	conn    net.Conn
	timeout time.Duration
}

func NewTimeoutConn(conn net.Conn, timeout time.Duration) *TimeoutConn {
	return &TimeoutConn{
		conn:    conn,
		timeout: timeout,
	}
}

func (c *TimeoutConn) Read(b []byte) (n int, err error) {
	c.SetReadDeadline(time.Now().Add(c.timeout))
	return c.conn.Read(b)
}

func (c *TimeoutConn) Write(b []byte) (n int, err error) {
	c.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.conn.Write(b)
}

func (c *TimeoutConn) Close() error {
	return c.conn.Close()
}

func (c *TimeoutConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *TimeoutConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *TimeoutConn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *TimeoutConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *TimeoutConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// Config is used in DialConfig and Open to specify the desired tuning
// parameters used during a connection open handshake.  The negotiated tuning
// will be stored in the returned connection's Config field.
type Config struct {
	// The SASL mechanisms to try in the client request, and the successful
	// mechanism used on the Connection object.  Dial sets this to the PlainAuth
	// from the URL.
	SASL []Authentication

	// Vhost specifies the namespace of permissions, exchanges, queues and
	// bindings on the server.  Dial sets this to the path parsed from the URL.
	Vhost string

	Channels  int           // 0 max channels means unlimited
	FrameSize int           // 0 max bytes means unlimited
	Heartbeat time.Duration // less than 1s interval means no heartbeats

	// TLSClientConfig specifies the client configuration of the TLS connection
	// when establishing a tls transport.
	TLSClientConfig *tls.Config

	// ConnectionTimeout specifies the duration to wait for Dialed TCP session to
	// be established.  ConnectionTimeout is also used as the initial read timout
	// for the AMQP connection handshake.
	ConnectionTimeout time.Duration
}

// Connection manages the serialization and deserialization of frames from IO
// and dispatches the frames to the appropriate channel.  All RPC methods and
// asyncronous Publishing, Delivery, Ack, Nack and Return messages are
// multiplexed on this channel.  There must always be active receivers for
// every asynchronous message on this connection.
type Connection struct {
	destructor sync.Once  // shutdown once
	sendM      sync.Mutex // conn writer mutex
	m          sync.Mutex // struct field mutex

	conn io.ReadWriteCloser

	rpc       chan message
	writer    *writer
	sends     chan time.Time     // timestamps of each frame sent
	deadlines chan readDeadliner // heartbeater updates read deadlines

	channels channelRegistry

	noNotify bool // true when we will never notify again
	closes   []chan *Error
	blocks   []chan Blocking

	errors chan *Error

	Config Config // The negotiated Config after connection.open

	Major      int   // Server's major version
	Minor      int   // Server's minor version
	Properties Table // Server properties
}

type readDeadliner interface {
	SetReadDeadline(time.Time) error
}

// Dial accepts a string in the AMQP URI format and returns a new Connection
// over TCP using PlainAuth.  Defaults to a server heartbeat interval of 10
// seconds and sets the initial read deadline to 30 seconds.
//
// Dial uses the zero value of tls.Config when it encounters an amqps://
// scheme.  It is equivalent to calling DialTLS(amqp, nil).
func Dial(url string) (*Connection, error) {
	return DialConfig(url, Config{
		Heartbeat:         defaultHeartbeat,
		ConnectionTimeout: defaultConnectionTimeout,
	})
}

// DialTLS accepts a string in the AMQP URI format and returns a new Connection
// over TCP using PlainAuth.  Defaults to a server heartbeat interval of 10
// seconds and sets the initial read deadline to 30 seconds.
//
// DialTLS uses the provided tls.Config when encountering an amqps:// scheme.
func DialTLS(url string, amqps *tls.Config) (*Connection, error) {
	return DialConfig(url, Config{
		Heartbeat:         defaultHeartbeat,
		ConnectionTimeout: defaultConnectionTimeout,
		TLSClientConfig:   amqps,
	})
}

// DialConfig accepts a string in the AMQP URI format and a configuration for
// the transport and connection setup, returning a new Connection.  Defaults to
// a server heartbeat interval of 10 seconds and sets the initial read deadline
// to 30 seconds.
func DialConfig(url string, config Config) (*Connection, error) {
	var err error
	var conn net.Conn

	uri, err := ParseURI(url)
	if err != nil {
		return nil, err
	}

	if config.SASL == nil {
		config.SASL = []Authentication{uri.PlainAuth()}
	}

	if config.Vhost == "" {
		config.Vhost = uri.Vhost
	}

	if uri.Scheme == "amqps" && config.TLSClientConfig == nil {
		config.TLSClientConfig = new(tls.Config)
	}

	addr := net.JoinHostPort(uri.Host, strconv.FormatInt(int64(uri.Port), 10))

    s_conn, err := net.DialTimeout("tcp", addr, config.ConnectionTimeout)
	if err != nil {
		return nil, err
	}

    conn = NewTimeoutConn(s_conn, readWriteTimeout)

	// Heartbeating hasn't started yet, don't stall forever on a dead server.
	if err := conn.SetReadDeadline(time.Now().Add(config.ConnectionTimeout)); err != nil {
		return nil, err
	}

	if config.TLSClientConfig != nil {
		// Use the URI's host for hostname validation unless otherwise set. Make a
		// copy so not to modify the caller's reference when the caller reuses a
		// tls.Config for a different URL.
		if config.TLSClientConfig.ServerName == "" {
			c := *config.TLSClientConfig
			c.ServerName = uri.Host
			config.TLSClientConfig = &c
		}

		client := tls.Client(conn, config.TLSClientConfig)
		if err := client.Handshake(); err != nil {
			conn.Close()
			return nil, err
		}

		conn = client
	}

	return Open(conn, config)
}

/*
Open accepts an already established connection, or other io.ReadWriteCloser as
a transport.  Use this method if you have established a TLS connection or wish
to use your own custom transport.

*/
func Open(conn io.ReadWriteCloser, config Config) (*Connection, error) {
	me := &Connection{
		conn:      conn,
		writer:    &writer{bufio.NewWriter(conn)},
		channels:  channelRegistry{channels: make(map[uint16]*Channel)},
		rpc:       make(chan message),
		sends:     make(chan time.Time),
		errors:    make(chan *Error, 1),
		deadlines: make(chan readDeadliner, 1),
	}
	go me.reader(conn)
	return me, me.open(config)
}

/*
NotifyClose registers a listener for close events either initiated by an error
accompaning a connection.close method or by a normal shutdown.

On normal shutdowns, the chan will be closed.

To reconnect after a transport or protocol error, register a listener here and
re-run your setup process.

*/
func (me *Connection) NotifyClose(c chan *Error) chan *Error {
	me.m.Lock()
	defer me.m.Unlock()

	if me.noNotify {
		close(c)
	} else {
		me.closes = append(me.closes, c)
	}

	return c
}

/*
NotifyBlock registers a listener for RabbitMQ specific TCP flow control method
extensions connection.blocked and connection.unblocked.  Flow control is active
with a reason when Blocking.Blocked is true.  When a Connection is blocked, all
methods will block across all connections until server resources become free
again.

This optional extension is supported by the server when the
"connection.blocked" server capability key is true.

*/
func (me *Connection) NotifyBlocked(c chan Blocking) chan Blocking {
	me.m.Lock()
	defer me.m.Unlock()

	if me.noNotify {
		close(c)
	} else {
		me.blocks = append(me.blocks, c)
	}

	return c
}

/*
Close requests and waits for the response to close the AMQP connection.

It's advisable to use this message when publishing to ensure all kernel buffers
have been flushed on the server and client before exiting.

An error indicates that server may not have received this request to close but
the connection should be treated as closed regardless.

After returning from this call, all resources associated with this connection,
including the underlying io, Channels, Notify listeners and Channel consumers
will also be closed.
*/
func (me *Connection) Close() error {
	defer me.shutdown(nil)
	return me.call(
		&connectionClose{
			ReplyCode: replySuccess,
			ReplyText: "kthxbai",
		},
		&connectionCloseOk{},
	)
}

func (me *Connection) closeWith(err *Error) error {
	defer me.shutdown(err)
	return me.call(
		&connectionClose{
			ReplyCode: uint16(err.Code),
			ReplyText: err.Reason,
		},
		&connectionCloseOk{},
	)
}

func (me *Connection) send(f frame) error {
	me.sendM.Lock()
	err := me.writer.WriteFrame(f)
	me.sendM.Unlock()

	if err != nil {
		// shutdown could be re-entrant from signaling notify chans
		me.shutdown(&Error{
			Code:   FrameError,
			Reason: err.Error(),
		})
	} else {
		// Broadcast we sent a frame, reducing heartbeats, only
		// if there is something that can receive - like a non-reentrant
		// call or if the heartbeater isn't running
		select {
		case me.sends <- time.Now():
		default:
		}
	}

	return err
}

func (me *Connection) shutdown(err *Error) {
	me.destructor.Do(func() {
		me.m.Lock()
		defer me.m.Unlock()

		if err != nil {
			for _, c := range me.closes {
				c <- err
			}
		}

		for _, ch := range me.channels.removeAll() {
			ch.shutdown(err)
		}

		if err != nil {
			me.errors <- err
		}

		me.conn.Close()

		for _, c := range me.closes {
			close(c)
		}

		for _, c := range me.blocks {
			close(c)
		}

		me.noNotify = true
	})
}

// All methods sent to the connection channel should be synchronous so we
// can handle them directly without a framing component
func (me *Connection) demux(f frame) {
	if f.channel() == 0 {
		me.dispatch0(f)
	} else {
		me.dispatchN(f)
	}
}

func (me *Connection) dispatch0(f frame) {
	switch mf := f.(type) {
	case *methodFrame:
		switch m := mf.Method.(type) {
		case *connectionClose:
			// Send immediately as shutdown will close our side of the writer.
			me.send(&methodFrame{
				ChannelId: 0,
				Method:    &connectionCloseOk{},
			})

			me.shutdown(newError(m.ReplyCode, m.ReplyText))
		case *connectionBlocked:
			for _, c := range me.blocks {
				c <- Blocking{Active: true, Reason: m.Reason}
			}
		case *connectionUnblocked:
			for _, c := range me.blocks {
				c <- Blocking{Active: false}
			}
		default:
			me.rpc <- m
		}
	case *heartbeatFrame:
		// kthx - all reads reset our deadline.  so we can drop this
	default:
		// lolwat - channel0 only responds to methods and heartbeats
		me.closeWith(ErrUnexpectedFrame)
	}
}

func (me *Connection) dispatchN(f frame) {
	if channel := me.channels.get(f.channel()); channel != nil {
		channel.recv(channel, f)
	} else {
		me.dispatchClosed(f)
	}
}

// section 2.3.7: "When a peer decides to close a channel or connection, it
// sends a Close method.  The receiving peer MUST respond to a Close with a
// Close-Ok, and then both parties can close their channel or connection.  Note
// that if peers ignore Close, deadlock can happen when both peers send Close
// at the same time."
//
// When we don't have a channel, so we must respond with close-ok on a close
// method.  This can happen between a channel exception on an asynchronous
// method like basic.publish and a synchronous close with channel.close.
// In that case, we'll get both a channel.close and channel.close-ok in any
// order.
func (me *Connection) dispatchClosed(f frame) {
	// Only consider method frames, drop content/header frames
	if mf, ok := f.(*methodFrame); ok {
		switch mf.Method.(type) {
		case *channelClose:
			me.send(&methodFrame{
				ChannelId: f.channel(),
				Method:    &channelCloseOk{},
			})
		case *channelCloseOk:
			// we are already closed, so do nothing
		default:
			// unexpected method on closed channel
			me.closeWith(ErrClosed)
		}
	}
}

// Reads each frame off the IO and hand off to the connection object that
// will demux the streams and dispatch to one of the opened channels or
// handle on channel 0 (the connection channel).
func (me *Connection) reader(r io.Reader) {
	buf := bufio.NewReader(r)
	frames := &reader{buf}
	conn, haveDeadliner := r.(readDeadliner)

	for {
		frame, err := frames.ReadFrame()

		if err != nil {
			me.shutdown(&Error{Code: FrameError, Reason: err.Error()})
			return
		}

		me.demux(frame)

		if haveDeadliner {
			me.deadlines <- conn
		}
	}
}

// Ensures that at least one frame is being sent at the tuned interval with a
// jitter tolerance of 1s
func (me *Connection) heartbeater(interval time.Duration, done chan *Error) {
	const maxServerHeartbeatsInFlight = 3

	var sendTicks <-chan time.Time
	if interval > 0 {
		sendTicks = time.Tick(interval)
	}

	lastSent := time.Now()

	for {
		select {
		case at, stillSending := <-me.sends:
			// When actively sending, depend on sent frames to reset server timer
			if stillSending {
				lastSent = at
			} else {
				return
			}

		case at := <-sendTicks:
			// When idle, fill the space with a heartbeat frame
			if at.Sub(lastSent) > interval-time.Second {
				if err := me.send(&heartbeatFrame{}); err != nil {
					// send heartbeats even after close/closeOk so we
					// tick until the connection starts erroring
					return
				}
			}

		case conn := <-me.deadlines:
			// When reading, reset our side of the deadline, if we've negotiated one with
			// a deadline that covers at least 2 server heartbeats
			if interval > 0 {
				conn.SetReadDeadline(time.Now().Add(maxServerHeartbeatsInFlight * interval))
			}

		case <-done:
			return
		}
	}
}

// Convienence method to inspect the Connection.Properties["capabilities"]
// Table for server identified capabilities like "basic.ack" or
// "confirm.select".
func (me *Connection) isCapable(featureName string) bool {
	capabilities, _ := me.Properties["capabilities"].(Table)
	hasFeature, _ := capabilities[featureName].(bool)
	return hasFeature
}

/*
Channel opens a unique, concurrent server channel to process the bulk of AMQP
messages.  Any error from methods on this receiver will render the receiver
invalid and a new Channel should be opened.

*/
func (me *Connection) Channel() (*Channel, error) {
	id := me.channels.next()
	channel := newChannel(me, id)
	me.channels.add(id, channel)
	return channel, channel.open()
}

func (me *Connection) call(req message, res ...message) error {
	// Special case for when the protocol header frame is sent insted of a
	// request method
	if req != nil {
		if err := me.send(&methodFrame{ChannelId: 0, Method: req}); err != nil {
			return err
		}
	}

	select {
	case err := <-me.errors:
		return err

	case msg := <-me.rpc:
		// Try to match one of the result types
		for _, try := range res {
			if reflect.TypeOf(msg) == reflect.TypeOf(try) {
				// *res = *msg
				vres := reflect.ValueOf(try).Elem()
				vmsg := reflect.ValueOf(msg).Elem()
				vres.Set(vmsg)
				return nil
			}
		}
		return ErrCommandInvalid
	}

	panic("unreachable")
}

//    Connection          = open-Connection *use-Connection close-Connection
//    open-Connection     = C:protocol-header
//                          S:START C:START-OK
//                          *challenge
//                          S:TUNE C:TUNE-OK
//                          C:OPEN S:OPEN-OK
//    challenge           = S:SECURE C:SECURE-OK
//    use-Connection      = *channel
//    close-Connection    = C:CLOSE S:CLOSE-OK
//                        / S:CLOSE C:CLOSE-OK
func (me *Connection) open(config Config) error {
	if err := me.send(&protocolHeader{}); err != nil {
		return err
	}

	return me.openStart(config)
}

func (me *Connection) openStart(config Config) error {
	start := &connectionStart{}

	if err := me.call(nil, start); err != nil {
		return err
	}

	me.Major = int(start.VersionMajor)
	me.Minor = int(start.VersionMinor)
	me.Properties = Table(start.ServerProperties)

	// eventually support challenge/response here by also responding to
	// connectionSecure.
	auth, ok := pickSASLMechanism(config.SASL, strings.Split(start.Mechanisms, " "))
	if !ok {
		return ErrSASL
	}

	// Save this mechanism off as the one we chose
	me.Config.SASL = []Authentication{auth}

	return me.openTune(config, auth)
}

func (me *Connection) openTune(config Config, auth Authentication) error {
	ok := &connectionStartOk{
		Mechanism: auth.Mechanism(),
		Response:  auth.Response(),
		ClientProperties: Table{ // Open an issue if you wish these refined/parameterizable
			"product": "https://github.com/streadway/amqp",
			"version": "β",
			"capabilities": Table{
				"connection.blocked": true,
			},
		},
	}
	tune := &connectionTune{}

	if err := me.call(ok, tune); err != nil {
		// per spec, a connection can only be closed when it has been opened
		// so at this point, we know it's an auth error, but the socket
		// was closed instead.  Return a meaningful error.
		return ErrCredentials
	}

	// When this is bounded, share the bound.  We're effectively only bounded
	// by MaxUint16.  If you hit a wrap around bug, throw a small party then
	// make an github issue.
	me.Config.Channels = pick(config.Channels, int(tune.ChannelMax))

	// Frame size includes headers and end byte (len(payload)+8), even if
	// this is less than FrameMinSize, use what the server sends because the
	// alternative is to stop the handshake here.
	me.Config.FrameSize = pick(config.FrameSize, int(tune.FrameMax))

	// Save this off for resetDeadline()
	me.Config.Heartbeat = time.Second * time.Duration(pick(
		int(config.Heartbeat/time.Second),
		int(tune.Heartbeat)))

	// "The client should start sending heartbeats after receiving a
	// Connection.Tune method"
	go me.heartbeater(me.Config.Heartbeat, me.NotifyClose(make(chan *Error, 1)))

	if err := me.send(&methodFrame{
		ChannelId: 0,
		Method: &connectionTuneOk{
			ChannelMax: uint16(me.Config.Channels),
			FrameMax:   uint32(me.Config.FrameSize),
			Heartbeat:  uint16(me.Config.Heartbeat / time.Second),
		},
	}); err != nil {
		return err
	}

	return me.openVhost(config)
}

func (me *Connection) openVhost(config Config) error {
	req := &connectionOpen{VirtualHost: config.Vhost}
	res := &connectionOpenOk{}

	if err := me.call(req, res); err != nil {
		// Cannot be closed yet, but we know it's a vhost problem
		return ErrVhost
	}

	me.Config.Vhost = config.Vhost

	return nil
}

func pick(client, server int) int {
	if client == 0 || server == 0 {
		// max
		if client > server {
			return client
		} else {
			return server
		}
	} else {
		// min
		if client > server {
			return server
		} else {
			return client
		}
	}
	panic("unreachable")
}
