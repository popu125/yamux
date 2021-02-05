package yamux

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

func (s *Session) handleWithRecover(keepalive bool) {
	defer func() {
		if e := recover(); e != nil {
			fmt.Printf("handleWithRecover panic: %v", e)
		}
	}()

	var retried int = 0
	defer func() {
		close(s.recvDoneCh)
		s.Close()
		s.conn.Close()
	}()

	for {
		var wg sync.WaitGroup
		s.exitCh = make(chan bool, 3)
		ctx, cancelFn := context.WithCancel(context.Background())

		wg.Add(2)
		go s.waitToDie(recv, ctx, &wg)
		go s.waitToDie(send, ctx, &wg)
		if keepalive {
			wg.Add(1)
			go s.waitToDie(keepaliveFn, ctx, &wg)
		}

		expected := <-s.exitCh
		cancelFn()
		close(s.disconnectCh)
		retried += 1
		wg.Wait()

		if expected || retried >= 3 {
			return
		}

		if s.wait <= 0 {
			return
		}
		timeout := time.NewTimer(s.wait)

		// Once reach here, the recv and send has dead,
		// wait to recover them
		// WARNING: There may be race if Ping() called close
		// and new Conn comes in at same time
		select {
		case c, ok := <-s.newConnCh:
			if !ok {
				return
			}
			// todo:lock
			s.conn = c
			// todo:save bufread state
			s.bufRead = bufio.NewReader(c)
			s.disconnectCh = make(chan struct{})
			retried = 0
			continue
		case <-timeout.C:
			return
		case <-s.shutdownCh:
			return
		}
	}
}

func recv(s *Session, ctx context.Context) error {
	bufRead := s.bufRead

	hdr := header(make([]byte, headerSize))
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		// Read the header
		if _, err := io.ReadFull(bufRead, hdr); err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "reset by peer") {
				s.logger.Printf("[ERR] yamux: Failed to read header: %v", err)
			}
			return err
		}

		// Verify the version
		if hdr.Version() != protoVersion {
			s.logger.Printf("[ERR] yamux: Invalid protocol version: %d", hdr.Version())
			return ErrInvalidVersion
		}

		mt := hdr.MsgType()
		if mt < typeData || mt > typeGoAway {
			return ErrInvalidMsgType
		}

		if err := handlers[mt](s, hdr); err != nil {
			return err
		}
	}

}

func send(s *Session, ctx context.Context) error {
	sendCh := s.sendCh
	conn := s.conn
	for {
		select {
		case ready := <-sendCh:
			// Send a header if ready
			if ready.Hdr != nil {
				sent := 0
				for sent < len(ready.Hdr) {
					n, err := conn.Write(ready.Hdr[sent:])
					if err != nil {
						s.logger.Printf("[ERR] yamux: Failed to write header: %v", err)
						asyncSendErr(ready.Err, err)
						return err
					}
					sent += n
				}
			}

			// Send data from a body if given
			if ready.Body != nil {
				_, err := io.Copy(s.conn, ready.Body)
				if err != nil {
					s.logger.Printf("[ERR] yamux: Failed to write body: %v", err)
					asyncSendErr(ready.Err, err)
					return err
				}
			}

			// No error, successful send
			asyncSendErr(ready.Err, nil)
		case <-s.shutdownCh:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func keepaliveFn(s *Session, ctx context.Context) error {
	for {
		select {
		case <-time.After(s.config.KeepAliveInterval):
			_, err := s.Ping()
			if err != nil {
				if err != ErrSessionShutdown {
					s.logger.Printf("[ERR] yamux: keepalive failed: %v", err)
					err = ErrKeepAliveTimeout
				} else {
					err = nil
				}
				return err
			}
		case <-s.shutdownCh:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *Session) waitToDie(fn func(s *Session, ctx context.Context) error, ctx context.Context, wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
		if e := recover(); e != nil {
			s.exitCh <- true
		}
	}()

	// return nil:should wait recover, error:should stop
	if err := fn(s, ctx); err == nil {
		s.exitCh <- true
	} else {
		s.exitCh <- false
	}
}

func (s *Session) DisconnectChan() <-chan struct{} {
	return s.disconnectCh
}

func (s *Session) ReplaceConn(conn net.Conn) {
	defer func() {
		recover()
	}() // Prevent panic if newConnCh is closed
	s.exitCh <- false
	if s.IsClosed() {
		return
	}
	s.newConnCh <- conn
}

func (s *Session) SaveMeta(info []byte) {
	s.metaLock.Lock()
	defer s.metaLock.Unlock()
	s.meta = info
}

func (s *Session) LoadMeta() []byte {
	s.metaLock.RLock()
	defer s.metaLock.RUnlock()
	return s.meta
}

func (s *Session) SetWait(du time.Duration) {
	s.wait = du
}
