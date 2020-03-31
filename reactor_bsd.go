// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build darwin netbsd freebsd openbsd dragonfly

package gnet

import "github.com/panjf2000/gnet/internal/netpoll"

func (svr *server) activateMainReactor() {
	defer svr.signalShutdown()

	switch svr.opts.LB {
	case Priority:
		go func() {
			ticker := time.NewTicker(time.Second)
			var (
				count = 0
			)
			defer ticker.Stop()
			for {
				<-ticker.C
				if count > 4 {
					count = 0
					svr.subLoopGroup.iterate(func(i int, e *eventloop) bool {
						atomic.StoreInt64(&e.priority, 1)
						return true
					})
				} else {
					svr.subLoopGroup.iterate(func(i int, e *eventloop) bool {
						var p = atomic.LoadInt64(&e.priority)
						p >>= 2
						if p < 1 {
							p = 1
						}
						atomic.StoreInt64(&e.priority, p)
						return true
					})
				}
				count++
			}
		}()
	default:
	}

	svr.logger.Printf("main reactor exits with error:%v\n", svr.mainLoop.poller.Polling(func(fd int, filter int16) error {
		return svr.acceptNewConnection(fd)
	}))
}

func (svr *server) activateSubReactor(el *eventloop) {
	defer svr.signalShutdown()

	if el.idx == 0 && svr.opts.Ticker {
		go el.loopTicker()
	}

	svr.logger.Printf("event-loop:%d exits with error:%v\n", el.idx, el.poller.Polling(func(fd int, filter int16) error {
		if c, ack := el.connections[fd]; ack {
			if filter == netpoll.EVFilterSock {
				return el.loopCloseConn(c, nil)
			}
			switch c.outboundBuffer.IsEmpty() {
			// Don't change the ordering of processing EVFILT_WRITE | EVFILT_READ | EV_ERROR/EV_EOF unless you're 100%
			// sure what you're doing!
			// Re-ordering can easily introduce bugs and bad side-effects, as I found out painfully in the past.
			case false:
				if filter == netpoll.EVFilterWrite {
					return el.loopWrite(c)
				}
				return nil
			case true:
				if filter == netpoll.EVFilterRead {
					return el.loopRead(c)
				}
				return nil
			}
		}
		return nil
	}))
}
