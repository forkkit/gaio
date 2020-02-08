// +build darwin netbsd freebsd openbsd dragonfly

package gaio

import (
	"sync"
	"syscall"
)

type poller struct {
	fd int
	// awaiting for poll
	awaiting      []int
	awaitingMutex sync.Mutex
	// closing signal
	die     chan struct{}
	dieOnce sync.Once
}

func openPoll() (*poller, error) {
	fd, err := syscall.Kqueue()
	if err != nil {
		return nil, err
	}

	_, err = syscall.Kevent(fd, []syscall.Kevent_t{{
		Ident:  0,
		Filter: syscall.EVFILT_USER,
		Flags:  syscall.EV_ADD | syscall.EV_CLEAR,
	}}, nil, nil)

	if err != nil {
		return nil, err
	}

	p := new(poller)
	p.fd = fd
	p.die = make(chan struct{})
	return p, nil
}

func (p *poller) Close() error {
	p.dieOnce.Do(func() {
		close(p.die)
	})
	return p.wakeup()
}

func (p *poller) Watch(fd int) error {
	p.awaitingMutex.Lock()
	p.awaiting = append(p.awaiting, fd)
	p.awaitingMutex.Unlock()

	return p.wakeup()
}

// wakeup interrupt kevent
func (p *poller) wakeup() error {
	// notify poller
	_, err := syscall.Kevent(p.fd, []syscall.Kevent_t{{
		Ident:  0,
		Filter: syscall.EVFILT_USER,
		Fflags: syscall.NOTE_TRIGGER,
	}}, nil, nil)
	return err
}

func (p *poller) Wait(chEventNotify chan pollerEvents) {
	events := make([]syscall.Kevent_t, maxEvents)
	defer func() {
		syscall.Close(p.fd)
	}()

	// kqueue eventloop
	var changes []syscall.Kevent_t
	for {
		select {
		case <-p.die:
			return
		default:
			p.awaitingMutex.Lock()
			for _, fd := range p.awaiting {
				changes = append(changes,
					syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_READ},
					syscall.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_ADD | syscall.EV_CLEAR, Filter: syscall.EVFILT_WRITE},
				)
			}
			p.awaiting = p.awaiting[:0]
			p.awaitingMutex.Unlock()

			// poll
			n, err := syscall.Kevent(p.fd, changes, events, nil)
			if err == syscall.EINTR {
				continue
			}
			if err != nil {
				return
			}
			changes = changes[:0]

			// event processing
			var pe pollerEvents
			for i := 0; i < n; i++ {
				ev := &events[i]
				if ev.Ident != 0 {
					e := event{ident: int(ev.Ident)}
					if ev.Filter == syscall.EVFILT_READ {
						e.r = true
						// https://golang.org/src/runtime/netpoll_kqueue.go
						// On some systems when the read end of a pipe
						// is closed the write end will not get a
						// _EVFILT_WRITE event, but will get a
						// _EVFILT_READ event with EV_EOF set.
						// Note that setting 'w' here just means that we
						// will wake up a goroutine waiting to write;
						// that goroutine will try the write again,
						// and the appropriate thing will happen based
						// on what that write returns (success, EPIPE, EAGAIN).
						if ev.Flags&syscall.EV_EOF != 0 {
							e.w = true
						}
					} else if ev.Filter == syscall.EVFILT_WRITE {
						e.w = true
					}

					pe = append(pe, e)
				}
			}

			select {
			case chEventNotify <- pe:
			case <-p.die:
				return
			}
		}
	}
}
