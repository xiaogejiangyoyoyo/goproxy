package msocks

import (
	"errors"
	"fmt"
	"io"
	"logging"
	"net"
	"sync"
	"time"
)

const (
	RETRY_TIMES    = 6
	CHANLEN        = 1024
	WIN_SIZE       = 256 * 1024
	ACKDELAY       = 100 * time.Millisecond
	IDLECLOSE      = 10 * time.Minute
	PINGTIME       = 30 * time.Second
	DIAL_TIMEOUT   = 30 * time.Second
	LOOKUP_TIMEOUT = 60 * time.Second
)

// use 1024 as default channel length, 1024 * 1024 = 1M
// that is the buffer before read, and it's the maxmium length of write window.
// default value of write window is 256K.
// that will be sent in 0.1s, so maxmium speed will be 2.56M/s = 20Mbps.

var logger logging.Logger

func init() {
	var err error
	logger, err = logging.NewFileLogger("default", -1, "msocks")
	if err != nil {
		panic(err)
	}
}

// TODO: ping/echo
type Session struct {
	flock sync.Mutex
	conn  net.Conn

	// lock ports before any ports op and id op
	plock   sync.Mutex
	next_id uint16
	ports   map[uint16]chan Frame

	on_conn func(string, string, uint16) (chan Frame, error)

	ch_ping chan int
}

func NewSession(conn net.Conn) (s *Session) {
	s = &Session{
		conn:    conn,
		ports:   make(map[uint16]chan Frame, 0),
		idle:    time.NewTicker(PINGTIME),
		ch_ping: make(chan int, 3),
	}
	logger.Noticef("session %p created.", s)
	go s.keep_eye_open()
	s.ch_ping <- 1
	return
}

func (s *Session) keep_eye_open() {
	for {
		timeout := time.After(6 * PINGTIME)
		select {
		case <-timeout:
			s.Close()
			return
		case <-s.ch_ping:
		PING:
			for {
				select {
				case <-s.ch_ping:
				default:
					break PING
				}
			}
			time.Sleep(PINGTIME)
			b := NewFrameNoParam(MSG_PING, 0)
			_, err = s.Write(b)
			if err != nil {
				logger.Err(err)
			}
		}
	}
}

func (s *Session) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *Session) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

var errClosing = "use of closed network connection"

func (s *Session) Write(b []byte) (n int, err error) {
	s.flock.Lock()
	defer s.flock.Unlock()
	n, err = s.conn.Write(b)
	switch {
	case err == nil:
	case err.Error() == errClosing:
		return 0, io.EOF
	default:
		return
	}
	if n != len(b) {
		err = io.ErrShortWrite
	}
	return
}

func (s *Session) Close() (err error) {
	logger.Warningf("close all(len:%d) for session: %p.", len(s.ports), s)
	defer s.conn.Close()
	for _, v := range s.ports {
		v <- nil
	}
	return
}

func (s *Session) PutIntoNextId(ch chan Frame) (id uint16, err error) {
	s.plock.Lock()
	defer s.plock.Unlock()

	startid := s.next_id
	_, ok := s.ports[s.next_id]
	for ok {
		s.next_id += 1
		if s.next_id == startid {
			err = errors.New("run out of stream id")
			logger.Err(err)
			return
		}
		_, ok = s.ports[s.next_id]
	}
	id = s.next_id
	s.next_id += 1
	logger.Debugf("put into next id(%d): %p.", id, ch)

	s.ports[id] = ch
	if s.idleclose != nil {
		s.idleclose.Stop()
		s.idleclose = nil
	}
	return
}

func (s *Session) PutIntoId(id uint16, ch chan Frame) {
	logger.Debugf("put into id(%d): %p.", id, ch)
	s.plock.Lock()
	defer s.plock.Unlock()

	s.ports[id] = ch
	return
}

func (s *Session) RemovePorts(streamid uint16) (err error) {
	logger.Noticef("remove ports: %p(%d).", s, streamid)
	s.plock.Lock()
	defer s.plock.Unlock()
	_, ok := s.ports[streamid]
	if ok {
		delete(s.ports, streamid)
	} else {
		err = fmt.Errorf("streamid(%d) not exist.", streamid)
	}
	if len(s.ports) == 0 {
		s.idleclose = time.AfterFunc(IDLECLOSE, func() {
			s.conn.Close()
		})
	}
	return
}

func (s *Session) Number() (n int) {
	return len(s.ports)
}

func (s *Session) on_syn(ft *FrameSyn) bool {
	_, ok := s.ports[ft.Streamid]
	if ok {
		logger.Err("frame sync stream id exist.")
		b := NewFrameOneInt(MSG_FAILED, ft.Streamid, ERR_IDEXIST)
		_, err := s.Write(b)
		if err != nil {
			return false
		}
		return true
	}

	// lock streamid temporary, do I need this?
	s.PutIntoId(ft.Streamid, nil)

	go func() {
		// TODO: timeout
		logger.Debugf("client(%p) try to connect: %s.", s, ft.Address)
		ch, err := s.on_conn("tcp", ft.Address, ft.Streamid)
		if err != nil {
			logger.Err(err)

			b := NewFrameOneInt(MSG_FAILED, ft.Streamid, ERR_CONNFAILED)
			_, err = s.Write(b)
			if err != nil {
				logger.Err(err)
				return
			}

			err = s.RemovePorts(ft.Streamid)
			if err != nil {
				logger.Err(err)
			}
			return
		}

		// update it, don't need to lock
		s.PutIntoId(ft.Streamid, ch)

		b := NewFrameNoParam(MSG_OK, ft.Streamid)
		_, err = s.Write(b)
		if err != nil {
			logger.Err(err)
			return
		}
		logger.Noticef("connected %p(%d) => %s.",
			s, ft.Streamid, ft.Address)
		return
	}()
	return true
}

func (s *Session) on_dns(ft *FrameDns) {
	// This will toke long time...
	go func() {
		ipaddr, err := net.LookupIP(ft.Hostname)
		if err != nil {
			logger.Err(err)
			ipaddr = make([]net.IP, 0)
		}

		b, err := NewFrameAddr(ft.Streamid, ipaddr)
		if err != nil {
			logger.Err(err)
			return
		}
		_, err = s.Write(b)
		if err != nil {
			logger.Err(err)
		}
		return
	}()
	return
}

// In all of situation, drop frame if chan full.
// And if frame finally come, drop it too.
func (s *Session) sendFrameInChan(f Frame) bool {
	streamid := f.GetStreamid()
	ch, ok := s.ports[streamid]
	if !ok {
		logger.Errf("%p(%d) not exist.", s, streamid)
		return true
	}
	select {
	case ch <- f:
		return true
	default:
		logger.Errf("%p(%d) chan has fulled.", s, streamid)
		return s.RemovePorts(streamid) == nil
	}
}

func (s *Session) Run() {
	defer s.Close()

	for {
		f, err := ReadFrame(s.conn)
		if err != nil {
			logger.Err(err)
			return
		}

		switch ft := f.(type) {
		default:
			logger.Err("unexpected package")
			return
		case *FrameOK, *FrameFAILED, *FrameData, *FrameAck, *FrameFin, *FrameAddr:
			f.Debug()
			if !s.sendFrameInChan(f) {
				return
			}
		case *FrameSyn:
			f.Debug()
			if !s.on_syn(ft) {
				return
			}
		case *FrameDns:
			f.Debug()
			go s.on_dns(ft)
		case *FramePing:
			f.Debug()
			select {
			case s.ch_ping <- 1:
			default:
			}
		}
	}
}