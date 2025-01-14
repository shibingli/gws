package gws

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"github.com/lxzan/gws/internal"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type Conn struct {
	// store session information
	SessionStorage SessionStorage
	// whether to use compression
	compressEnabled bool
	// tcp connection
	conn net.Conn
	// server configs
	config *Config
	// read buffer
	rbuf *bufio.Reader
	// flate decompressor
	decompressor *decompressor
	// continuation frame
	continuationFrame continuationFrame
	// frame header for read
	fh frameHeader
	// write buffer
	wbuf *bufio.Writer
	// flate compressor
	compressor *compressor
	// WebSocket Event Handler
	handler Event

	// whether server is closed
	closed uint32
	// write lock
	wmu sync.Mutex
	// async read task queue
	readQueue workerQueue
	// async write task queue
	writeQueue workerQueue
}

func serveWebSocket(config *Config, session SessionStorage, netConn net.Conn, brw *bufio.ReadWriter, handler Event, compressEnabled bool) *Conn {
	c := &Conn{
		SessionStorage:  session,
		config:          config,
		compressEnabled: compressEnabled,
		conn:            netConn,
		closed:          0,
		wbuf:            brw.Writer,
		wmu:             sync.Mutex{},
		rbuf:            brw.Reader,
		fh:              frameHeader{},
		handler:         handler,
		readQueue:       workerQueue{maxConcurrency: int32(config.ReadAsyncGoLimit), capacity: config.ReadAsyncCap},
		writeQueue:      workerQueue{maxConcurrency: 1, capacity: config.WriteAsyncCap},
	}

	if c.compressEnabled {
		c.compressor = newCompressor(config.CompressLevel)
		c.decompressor = newDecompressor()
	}

	return c
}

// Listen listening to websocket messages through a dead loop
// 监听websocket消息
func (c *Conn) Listen() {
	defer c.conn.Close()

	c.handler.OnOpen(c)
	for {
		if err := c.readMessage(); err != nil {
			c.emitError(err)
			return
		}
	}
}

func (c *Conn) emitError(err error) {
	if err == nil {
		return
	}

	var responseCode = internal.CloseNormalClosure
	var responseErr error = internal.CloseNormalClosure
	switch v := err.(type) {
	case internal.StatusCode:
		responseCode = v
	case *internal.Error:
		responseCode = v.Code
		responseErr = v.Err
	default:
		responseErr = err
	}

	var content = responseCode.Bytes()
	content = append(content, err.Error()...)
	if len(content) > internal.ThresholdV1 {
		content = content[:internal.ThresholdV1]
	}
	if atomic.CompareAndSwapUint32(&c.closed, 0, 1) {
		_ = c.doWrite(OpcodeCloseConnection, content)
		c.handler.OnError(c, responseErr)
	}
}

func (c *Conn) emitClose(buf *bytes.Buffer) error {
	var responseCode = internal.CloseNormalClosure
	var realCode = internal.CloseNormalClosure.Uint16()
	switch buf.Len() {
	case 0:
		responseCode = 0
		realCode = 0
	case 1:
		responseCode = internal.CloseProtocolError
		realCode = uint16(buf.Bytes()[0])
		buf.Reset()
	default:
		var b [2]byte
		_, _ = buf.Read(b[0:])
		realCode = binary.BigEndian.Uint16(b[0:])
		switch realCode {
		case 1004, 1005, 1006, 1014, 1015:
			responseCode = internal.CloseProtocolError
		default:
			if realCode < 1000 || realCode >= 5000 || (realCode >= 1016 && realCode < 3000) {
				responseCode = internal.CloseProtocolError
			} else if realCode < 1016 {
				responseCode = internal.CloseNormalClosure
			} else {
				responseCode = internal.StatusCode(realCode)
			}
		}
		if c.config.CheckUtf8Enabled && !isTextValid(OpcodeCloseConnection, buf.Bytes()) {
			responseCode = internal.CloseUnsupportedData
		}
	}
	if atomic.CompareAndSwapUint32(&c.closed, 0, 1) {
		_ = c.doWrite(OpcodeCloseConnection, responseCode.Bytes())
		c.handler.OnClose(c, realCode, buf.Bytes())
	}
	return internal.CloseNormalClosure
}

// SetDeadline sets deadline
func (c *Conn) SetDeadline(t time.Time) error {
	err := c.conn.SetDeadline(t)
	c.emitError(err)
	return err
}

// SetReadDeadline sets read deadline
func (c *Conn) SetReadDeadline(t time.Time) error {
	err := c.conn.SetReadDeadline(t)
	c.emitError(err)
	return err
}

// SetWriteDeadline sets write deadline
func (c *Conn) SetWriteDeadline(t time.Time) error {
	err := c.conn.SetWriteDeadline(t)
	c.emitError(err)
	return err
}

func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// NetConn get tcp/tls/... conn
func (c *Conn) NetConn() net.Conn {
	return c.conn
}

// setNoDelay set tcp no delay
func setNoDelay(conn net.Conn) error {
	switch v := conn.(type) {
	case *net.TCPConn:
		return v.SetNoDelay(false)
	case *tls.Conn:
		if netConn, ok := conn.(internal.NetConn); ok {
			return setNoDelay(netConn.NetConn())
		}
	}
	return nil
}
