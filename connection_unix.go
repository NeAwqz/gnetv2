// Copyright (c) 2019 The Gnet Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux || freebsd || dragonfly || netbsd || openbsd || darwin
// +build linux freebsd dragonfly netbsd openbsd darwin

package gnet

import (
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/panjf2000/gnet/v2/internal/bs"
	"github.com/panjf2000/gnet/v2/internal/gfd"
	gio "github.com/panjf2000/gnet/v2/internal/io"
	"github.com/panjf2000/gnet/v2/internal/netpoll"
	"github.com/panjf2000/gnet/v2/internal/queue"
	"github.com/panjf2000/gnet/v2/internal/socket"
	"github.com/panjf2000/gnet/v2/pkg/buffer/elastic"
	"github.com/panjf2000/gnet/v2/pkg/buffer/linkedlist"
	errorx "github.com/panjf2000/gnet/v2/pkg/errors"
	"github.com/panjf2000/gnet/v2/pkg/logging"
	bsPool "github.com/panjf2000/gnet/v2/pkg/pool/byteslice"
)

type conn struct {
	fd             int                    // file descriptor
	gfd            gfd.GFD                // gnet file descriptor
	ctx            interface{}            // user-defined context
	peer           unix.Sockaddr          // remote socket address
	localAddr      net.Addr               // local addr
	remoteAddr     net.Addr               // remote addr
	loop           *eventloop             // connected event-loop
	outboundBuffer elastic.Buffer         // buffer for data that is eligible to be sent to the peer
	pollAttachment netpoll.PollAttachment // connection attachment for poller
	inboundBuffer  elastic.RingBuffer     // buffer for leftover data from the peer
	elasticBuffer  linkedlist.Buffer      // elastic buffer for ET I/O
	buffer         []byte                 // buffer for the latest bytes
	isDatagram     bool                   // UDP protocol
	opened         bool                   // connection opened event fired
}

func newTCPConn(fd int, el *eventloop, sa unix.Sockaddr, localAddr, remoteAddr net.Addr) (c *conn) {
	c = &conn{
		fd:             fd,
		peer:           sa,
		loop:           el,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
		pollAttachment: netpoll.PollAttachment{FD: fd},
	}
	c.pollAttachment.Callback = c.handleEvents
	c.outboundBuffer.Reset(el.engine.opts.WriteBufferCap)
	return
}

func newUDPConn(fd int, el *eventloop, localAddr net.Addr, sa unix.Sockaddr, connected bool) (c *conn) {
	c = &conn{
		fd:             fd,
		gfd:            gfd.NewGFD(fd, el.idx, 0, 0),
		peer:           sa,
		loop:           el,
		localAddr:      localAddr,
		remoteAddr:     socket.SockaddrToUDPAddr(sa),
		isDatagram:     true,
		pollAttachment: netpoll.PollAttachment{FD: fd, Callback: el.readUDP},
	}
	if connected {
		c.peer = nil
	}
	return
}

func (c *conn) release() {
	c.opened = false
	c.ctx = nil
	c.buffer = nil
	if addr, ok := c.localAddr.(*net.TCPAddr); ok && c.localAddr != c.loop.ln.addr && len(addr.Zone) > 0 {
		bsPool.Put(bs.StringToBytes(addr.Zone))
	}
	if addr, ok := c.remoteAddr.(*net.TCPAddr); ok && len(addr.Zone) > 0 {
		bsPool.Put(bs.StringToBytes(addr.Zone))
	}
	if addr, ok := c.localAddr.(*net.UDPAddr); ok && c.localAddr != c.loop.ln.addr && len(addr.Zone) > 0 {
		bsPool.Put(bs.StringToBytes(addr.Zone))
	}
	if addr, ok := c.remoteAddr.(*net.UDPAddr); ok && len(addr.Zone) > 0 {
		bsPool.Put(bs.StringToBytes(addr.Zone))
	}
	c.localAddr = nil
	c.remoteAddr = nil
	if !c.isDatagram {
		c.peer = nil
		c.inboundBuffer.Done()
		c.outboundBuffer.Release()
	}
}

func (c *conn) open(buf []byte) error {
	if c.isDatagram && c.peer == nil {
		return unix.Send(c.fd, buf, 0)
	}

	for {
		n, err := unix.Write(c.fd, buf)
		if err != nil {
			if err == unix.EAGAIN {
				_, _ = c.outboundBuffer.Write(buf)
				break
			}
			return err
		}
		buf = buf[n:]
		if len(buf) == 0 {
			break
		}
	}

	return nil
}

func (c *conn) writeLT(data []byte) (n int, err error) {
	n = len(data)
	// If there is pending data in outbound buffer, the current data ought to be appended to the outbound buffer
	// for maintaining the sequence of network packets.
	if !c.outboundBuffer.IsEmpty() {
		_, _ = c.outboundBuffer.Write(data)
		return
	}

	var sent int
	if sent, err = unix.Write(c.fd, data); err != nil {
		// A temporary error occurs, append the data to outbound buffer, writing it back to the peer in the next round.
		if err == unix.EAGAIN {
			_, _ = c.outboundBuffer.Write(data)
			err = c.loop.poller.ModReadWrite(&c.pollAttachment, false)
			return
		}
		if err := c.loop.close(c, os.NewSyscallError("write", err)); err != nil {
			logging.Errorf("failed to close connection(fd=%d,peer=%+v) on conn.write: %v",
				c.fd, c.remoteAddr, err)
		}
		return 0, os.NewSyscallError("write", err)
	}
	// Failed to send all data back to the peer, buffer the leftover data for the next round.
	if sent < n {
		_, _ = c.outboundBuffer.Write(data[sent:])
		err = c.loop.poller.ModReadWrite(&c.pollAttachment, false)
	}
	return
}

func (c *conn) writeET(data []byte) (n int, err error) {
	n = len(data)
	// If there is pending data in outbound buffer, the current data ought to be appended to the outbound buffer
	// for maintaining the sequence of network packets.
	if !c.outboundBuffer.IsEmpty() {
		_, _ = c.outboundBuffer.Write(data)
		return n, c.loop.writeET(c)
	}

	remaining := n
	for {
		n, err := unix.Write(c.fd, data)
		if err != nil {
			// A temporary error occurs, append the data to outbound buffer, writing it back to the peer in the next round.
			if err == unix.EAGAIN {
				_, _ = c.outboundBuffer.Write(data)
				break
			}
			if err := c.loop.close(c, os.NewSyscallError("write", err)); err != nil {
				logging.Errorf("failed to close connection(fd=%d,peer=%+v) on conn.write: %v",
					c.fd, c.remoteAddr, err)
			}
			return 0, os.NewSyscallError("write", err)
		}
		remaining -= n
		data = data[n:]
		if remaining == 0 {
			break
		}
	}
	return
}

func (c *conn) writevLT(bs [][]byte) (n int, err error) {
	for _, b := range bs {
		n += len(b)
	}

	// If there is pending data in outbound buffer, the current data ought to be appended to the outbound buffer
	// for maintaining the sequence of network packets.
	if !c.outboundBuffer.IsEmpty() {
		_, _ = c.outboundBuffer.Writev(bs)
		return
	}

	var sent int
	if sent, err = gio.Writev(c.fd, bs); err != nil {
		// A temporary error occurs, append the data to outbound buffer, writing it back to the peer in the next round.
		if err == unix.EAGAIN {
			_, _ = c.outboundBuffer.Writev(bs)
			err = c.loop.poller.ModReadWrite(&c.pollAttachment, false)
			return
		}
		if err := c.loop.close(c, os.NewSyscallError("writev", err)); err != nil {
			logging.Errorf("failed to close connection(fd=%d,peer=%+v) on conn.writev: %v",
				c.fd, c.remoteAddr, err)
		}
		return 0, os.NewSyscallError("writev", err)
	}
	// Failed to send all data back to the peer, buffer the leftover data for the next round.
	if sent < n {
		var pos int
		for i := range bs {
			bn := len(bs[i])
			if sent < bn {
				bs[i] = bs[i][sent:]
				pos = i
				break
			}
			sent -= bn
		}
		_, _ = c.outboundBuffer.Writev(bs[pos:])
		err = c.loop.poller.ModReadWrite(&c.pollAttachment, false)
	}
	return
}

func (c *conn) writevET(bs [][]byte) (n int, err error) {
	for _, b := range bs {
		n += len(b)
	}

	// If there is pending data in outbound buffer, the current data ought to be appended to the outbound buffer
	// for maintaining the sequence of network packets.
	if !c.outboundBuffer.IsEmpty() {
		_, _ = c.outboundBuffer.Writev(bs)
		return n, c.loop.writeET(c)
	}

	remaining := n
	for {
		n, err := gio.Writev(c.fd, bs)
		if err != nil {
			// A temporary error occurs, append the data to outbound buffer, writing it back to the peer in the next round.
			if err == unix.EAGAIN {
				_, _ = c.outboundBuffer.Writev(bs)
				break
			}
			if err := c.loop.close(c, os.NewSyscallError("writev", err)); err != nil {
				logging.Errorf("failed to close connection(fd=%d,peer=%+v) on conn.writev: %v",
					c.fd, c.remoteAddr, err)
			}
			return 0, os.NewSyscallError("writev", err)
		}
		remaining -= n
		var pos int
		for i := range bs {
			bn := len(bs[i])
			if n < bn {
				bs[i] = bs[i][n:]
				pos = i
				break
			}
			n -= bn
		}
		bs = bs[pos:]
		if remaining == 0 {
			break
		}
	}
	return
}

type asyncWriteHook struct {
	callback AsyncCallback
	data     []byte
}

func (c *conn) asyncWrite(itf interface{}) (err error) {
	hook := itf.(*asyncWriteHook)
	defer func() {
		if hook.callback != nil {
			_ = hook.callback(c, err)
		}
	}()

	if !c.opened {
		return net.ErrClosed
	}

	if c.loop.engine.opts.EdgeTriggeredIO {
		_, err = c.writeET(hook.data)
		return
	}
	_, err = c.writeLT(hook.data)
	return
}

type asyncWritevHook struct {
	callback AsyncCallback
	data     [][]byte
}

func (c *conn) asyncWritev(itf interface{}) (err error) {
	hook := itf.(*asyncWritevHook)
	defer func() {
		if hook.callback != nil {
			_ = hook.callback(c, err)
		}
	}()

	if !c.opened {
		return net.ErrClosed
	}

	if c.loop.engine.opts.EdgeTriggeredIO {
		_, err = c.writevET(hook.data)
		return
	}
	_, err = c.writevLT(hook.data)
	return
}

func (c *conn) sendTo(buf []byte) error {
	if c.peer == nil {
		return unix.Send(c.fd, buf, 0)
	}
	return unix.Sendto(c.fd, buf, 0, c.peer)
}

func (c *conn) resetBuffer() {
	c.buffer = c.buffer[:0]
	c.inboundBuffer.Reset()
}

func (c *conn) Read(p []byte) (n int, err error) {
	if c.loop.engine.opts.EdgeTriggeredIO {
		return c.elasticBuffer.Read(p)
	}

	if c.inboundBuffer.IsEmpty() {
		n = copy(p, c.buffer)
		c.buffer = c.buffer[n:]
		if n == 0 && len(p) > 0 {
			err = io.ErrShortBuffer
		}
		return
	}
	n, _ = c.inboundBuffer.Read(p)
	if n == len(p) {
		return
	}
	m := copy(p[n:], c.buffer)
	n += m
	c.buffer = c.buffer[m:]
	return
}

func (c *conn) Next(n int) (buf []byte, err error) {
	if c.loop.engine.opts.EdgeTriggeredIO {
		if n == -1 || n > c.elasticBuffer.Buffered() {
			n = c.elasticBuffer.Buffered()
		}
		c.loop.cache.Reset()
		_, err = c.loop.cache.ReadFrom(io.LimitReader(&c.elasticBuffer, int64(n)))
		return c.loop.cache.Bytes(), err
	}

	inBufferLen := c.inboundBuffer.Buffered()
	if totalLen := inBufferLen + len(c.buffer); n > totalLen {
		return nil, io.ErrShortBuffer
	} else if n <= 0 {
		n = totalLen
	}
	if c.inboundBuffer.IsEmpty() {
		buf = c.buffer[:n]
		c.buffer = c.buffer[n:]
		return
	}
	head, tail := c.inboundBuffer.Peek(n)
	defer c.inboundBuffer.Discard(n) //nolint:errcheck
	if len(head) >= n {
		return head[:n], err
	}
	c.loop.cache.Reset()
	c.loop.cache.Write(head)
	c.loop.cache.Write(tail)
	if inBufferLen >= n {
		return c.loop.cache.Bytes(), err
	}

	remaining := n - inBufferLen
	c.loop.cache.Write(c.buffer[:remaining])
	c.buffer = c.buffer[remaining:]
	return c.loop.cache.Bytes(), err
}

func (c *conn) Peek(n int) (buf []byte, err error) {
	if c.loop.engine.opts.EdgeTriggeredIO {
		bs, err := c.elasticBuffer.Peek(n)
		if len(bs) == 1 {
			return bs[0], err
		}
		c.loop.cache.Reset()
		for _, b := range bs {
			c.loop.cache.Write(b)
		}
		return c.loop.cache.Bytes(), err
	}

	inBufferLen := c.inboundBuffer.Buffered()
	if totalLen := inBufferLen + len(c.buffer); n > totalLen {
		return nil, io.ErrShortBuffer
	} else if n <= 0 {
		n = totalLen
	}
	if c.inboundBuffer.IsEmpty() {
		return c.buffer[:n], err
	}
	head, tail := c.inboundBuffer.Peek(n)
	if len(head) >= n {
		return head[:n], err
	}
	c.loop.cache.Reset()
	c.loop.cache.Write(head)
	c.loop.cache.Write(tail)
	if inBufferLen >= n {
		return c.loop.cache.Bytes(), err
	}

	remaining := n - inBufferLen
	c.loop.cache.Write(c.buffer[:remaining])
	return c.loop.cache.Bytes(), err
}

func (c *conn) Discard(n int) (int, error) {
	if c.loop.engine.opts.EdgeTriggeredIO {
		return c.elasticBuffer.Discard(n)
	}

	inBufferLen := c.inboundBuffer.Buffered()
	tempBufferLen := len(c.buffer)
	if inBufferLen+tempBufferLen < n || n <= 0 {
		c.resetBuffer()
		return inBufferLen + tempBufferLen, nil
	}
	if c.inboundBuffer.IsEmpty() {
		c.buffer = c.buffer[n:]
		return n, nil
	}

	discarded, _ := c.inboundBuffer.Discard(n)
	if discarded < inBufferLen {
		return discarded, nil
	}

	remaining := n - inBufferLen
	c.buffer = c.buffer[remaining:]
	return n, nil
}

func (c *conn) Write(p []byte) (int, error) {
	if c.isDatagram {
		if err := c.sendTo(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if c.loop.engine.opts.EdgeTriggeredIO {
		return c.writeET(p)
	}
	return c.writeLT(p)
}

func (c *conn) Writev(bs [][]byte) (int, error) {
	if c.isDatagram {
		return 0, errorx.ErrUnsupportedOp
	}
	if c.loop.engine.opts.EdgeTriggeredIO {
		return c.writevET(bs)
	}
	return c.writevLT(bs)
}

func (c *conn) ReadFrom(r io.Reader) (int64, error) {
	if c.loop.engine.opts.EdgeTriggeredIO {
		return c.elasticBuffer.ReadFrom(r)
	}

	return c.outboundBuffer.ReadFrom(r)
}

func (c *conn) WriteTo(w io.Writer) (n int64, err error) {
	if c.loop.engine.opts.EdgeTriggeredIO {
		return c.elasticBuffer.WriteTo(w)
	}

	if !c.inboundBuffer.IsEmpty() {
		if n, err = c.inboundBuffer.WriteTo(w); err != nil {
			return
		}
	}
	var m int
	m, err = w.Write(c.buffer)
	n += int64(m)
	c.buffer = c.buffer[m:]
	return
}

func (c *conn) Flush() error {
	if c.outboundBuffer.IsEmpty() {
		return nil
	}

	return c.loop.write(c)
}

func (c *conn) InboundBuffered() int {
	if c.loop.engine.opts.EdgeTriggeredIO {
		return c.elasticBuffer.Buffered()
	}

	return c.inboundBuffer.Buffered() + len(c.buffer)
}

func (c *conn) OutboundBuffered() int {
	return c.outboundBuffer.Buffered()
}

func (c *conn) Context() interface{}       { return c.ctx }
func (c *conn) SetContext(ctx interface{}) { c.ctx = ctx }
func (c *conn) LocalAddr() net.Addr        { return c.localAddr }
func (c *conn) RemoteAddr() net.Addr       { return c.remoteAddr }

// Implementation of Socket interface

// func (c *conn) Gfd() gfd.GFD             { return c.gfd }

func (c *conn) Fd() int                        { return c.fd }
func (c *conn) Dup() (fd int, err error)       { fd, _, err = netpoll.Dup(c.fd); return }
func (c *conn) SetReadBuffer(bytes int) error  { return socket.SetRecvBuffer(c.fd, bytes) }
func (c *conn) SetWriteBuffer(bytes int) error { return socket.SetSendBuffer(c.fd, bytes) }
func (c *conn) SetLinger(sec int) error        { return socket.SetLinger(c.fd, sec) }
func (c *conn) SetNoDelay(noDelay bool) error {
	return socket.SetNoDelay(c.fd, func(b bool) int {
		if b {
			return 1
		}
		return 0
	}(noDelay))
}

func (c *conn) SetKeepAlivePeriod(d time.Duration) error {
	return socket.SetKeepAlivePeriod(c.fd, int(d.Seconds()))
}

func (c *conn) AsyncWrite(buf []byte, callback AsyncCallback) error {
	if c.isDatagram {
		err := c.sendTo(buf)
		// TODO: it will not go asynchronously with UDP, so calling a callback is needless,
		//  we may remove this branch in the future, please don't rely on the callback
		// 	to do something important under UDP, if you're working with UDP, just call Conn.Write
		// 	to send back your data.
		if callback != nil {
			_ = callback(nil, nil)
		}
		return err
	}
	return c.loop.poller.Trigger(queue.HighPriority, c.asyncWrite, &asyncWriteHook{callback, buf})
}

func (c *conn) AsyncWritev(bs [][]byte, callback AsyncCallback) error {
	if c.isDatagram {
		return errorx.ErrUnsupportedOp
	}
	return c.loop.poller.Trigger(queue.HighPriority, c.asyncWritev, &asyncWritevHook{callback, bs})
}

func (c *conn) Wake(callback AsyncCallback) error {
	return c.loop.poller.Trigger(queue.LowPriority, func(_ interface{}) (err error) {
		err = c.loop.wake(c)
		if callback != nil {
			_ = callback(c, err)
		}
		return
	}, nil)
}

func (c *conn) CloseWithCallback(callback AsyncCallback) error {
	return c.loop.poller.Trigger(queue.LowPriority, func(_ interface{}) (err error) {
		err = c.loop.close(c, nil)
		if callback != nil {
			_ = callback(c, err)
		}
		return
	}, nil)
}

func (c *conn) Close() error {
	return c.loop.poller.Trigger(queue.LowPriority, func(_ interface{}) (err error) {
		err = c.loop.close(c, nil)
		return
	}, nil)
}

func (*conn) SetDeadline(_ time.Time) error {
	return errorx.ErrUnsupportedOp
}

func (*conn) SetReadDeadline(_ time.Time) error {
	return errorx.ErrUnsupportedOp
}

func (*conn) SetWriteDeadline(_ time.Time) error {
	return errorx.ErrUnsupportedOp
}
