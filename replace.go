package yamux

import (
	"net"
	"sync"
)

func (s *Session) handleWithRecover(keepalive bool) {
	var once sync.Once
	for {
		var wg sync.WaitGroup
		go s.waitToDie(recv, &wg)
		go s.waitToDie(send, &wg)
		if keepalive {
			once.Do(func() {
				go s.keepalive()
			})
		}
		wg.Wait()

		// Once reach here, the recv and send has dead,
		// wait to recover them
		// WARNING: There may be race if Ping() called close
		// and new Conn comes in at same time
		select {
		case c, ok := <-s.newConnCh:
			if !ok {
				return
			}
			s.conn = c
			continue
		case <-s.shutdownCh:
			return
		}
	}
}

func recv(s *Session) {
	s.recvLoop()
}

func send(s *Session) {
	s.send()
}

func (s *Session) waitToDie(fn func(s *Session), wg *sync.WaitGroup) {
	wg.Add(1)
	fn(s)
	wg.Done()
}

func (s *Session) ReplaceConn(conn net.Conn) {
	<-s.recvDoneCh
	s.recvDoneCh = make(chan struct{})
	s.newConnCh <- conn
}
