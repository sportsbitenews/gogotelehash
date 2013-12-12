package telehash

import (
	"fmt"
	"github.com/fd/go-util/log"
	"sync/atomic"
	"time"
)

type line_t struct {
	sw            *Switch
	peer          *Peer
	log           log.Logger
	shutdown      chan bool
	snd_chan      chan cmd_line_snd
	snd_open_chan chan cmd_open_snd
	rcv_open_chan chan cmd_open_rcv // buffered rcv channel
	rcv_line_chan chan *pkt_t       // buffered rcv channel
	prv_key       *private_line_key
	pub_key       *public_line_key
	shr_key       *shared_line_key
	state         line_state
	err           error
	open_retries  int

	channels         map[string]*channel_t
	add_channel_chan chan *channel_t
}

type (
	cmd_line_snd struct {
		pkt   *pkt_t
		reply chan error
	}

	cmd_open_rcv struct {
		pub     *public_line_key
		netpath NetPath
	}

	cmd_open_snd struct {
		netpath NetPath
	}
)

func (l *line_t) Init(sw *Switch, peer *Peer) {
	l.sw = sw
	l.peer = peer
	l.log = sw.log.Sub(log_level_for("LINE", log.DEFAULT), "line["+l.peer.Hashname().Short()+"]")
	l.open_retries = 3

	l.snd_chan = make(chan cmd_line_snd, 10)
	l.snd_open_chan = make(chan cmd_open_snd, 10)
	l.rcv_open_chan = make(chan cmd_open_rcv, 10)
	l.rcv_line_chan = make(chan *pkt_t, 10)

	l.channels = make(map[string]*channel_t, 10)
	l.add_channel_chan = make(chan *channel_t, 1)
}

// atomically get the line state
func (l *line_t) State() line_state {
	return line_state(atomic.LoadUint32((*uint32)(&l.state)))
}

func (l *line_t) LastErr() error {
	return l.err
}

// atomically start the line when necessary
func (l *line_t) EnsureRunning() error {
	for {
		old_state := l.State()

		if old_state.test(line_broken, 0) {
			return ErrPeerBroken
		}

		if old_state.test(line_running, 0) {
			return nil
		}

		new_state := old_state
		new_state.mod(line_running, 0)

		if atomic.CompareAndSwapUint32((*uint32)(&l.state), uint32(old_state), uint32(new_state)) {
			l.shutdown = make(chan bool, 1)
			go l.run_main_loop()
			return nil
		}
	}
}

func (l *line_t) Shutdown() {
	select {
	case l.shutdown <- true:
	default:
	}
}

func (l *line_t) Snd(pkt *pkt_t) error {
	err := l.EnsureRunning()
	if err != nil {
		return err
	}

	reply := make(chan error)
	l.snd_chan <- cmd_line_snd{pkt, reply}
	return <-reply
}

func (l *line_t) SndOpen(p NetPath) {
	if l.State().test(0, line_running) {
		return // drop
	}

	l.snd_open_chan <- cmd_open_snd{p}
}

func (l *line_t) RcvLine(pkt *pkt_t) {
	if l.State().test(0, line_running) {
		return // drop
	}

	l.rcv_line_chan <- pkt
}

func (l *line_t) RcvOpen(pub *public_line_key, netpath NetPath) {
	err := l.EnsureRunning()
	if err != nil {
		return // drop
	}

	l.rcv_open_chan <- cmd_open_rcv{pub, netpath}
}

func (l *line_t) OpenChannel(pkt *pkt_t, raw bool) (*channel_t, error) {
	channel, err := make_channel(l.sw, l, "", pkt.hdr.Type, true, raw)
	if err != nil {
		return nil, err
	}

	l.add_channel_chan <- channel

	channel.log.Debugf("channel[%s:%s](%s -> %s): opened",
		short_hash(channel.channel_id),
		pkt.hdr.Type,
		l.sw.hashname.Short(),
		l.peer.Hashname().Short())

	err = channel.snd_pkt(pkt)
	if err != nil {
		return nil, err
	}

	return channel, nil
}

func (l *line_t) run_main_loop() {
	defer l.teardown()

	if !l.handle_err(l.setup()) {
		return
	}

	for {
		switch {

		case l.state.test(line_terminating, 0):
			return

		case l.state.test(line_opened, 0):
			l.run_line_loop()

		case l.state.test(0, line_active):
			if l.open_retries > 0 {
				l.state = line_running
				l.start_opening()
			} else {
				l.state.mod(line_peer_down, 0)
				return
			}

		case l.state.test(line_peering, 0):
			l.run_peer_loop()

		case l.state.test(line_opening, 0):
			l.run_open_loop()

		}
	}
}

func (l *line_t) setup() error {
	l.log.Debugf("started running line")

	l.register()

	l.start_opening()

	return nil
}

func (l *line_t) start_opening() {
	l.open_retries--

	if l.peer.HasVia() {
		l.log.Noticef("opening line to=%s (via)", l.peer.Hashname().Short())
		l.state.mod(line_opening|line_peering, 0)
	} else {
		l.log.Noticef("opening line to=%s", l.peer.Hashname().Short())
		l.state.mod(line_opening, 0)
	}
}

func (l *line_t) run_peer_loop() {
	l.sw.peer_handler.SendPeer(l.peer)

	var (
		timeout_d = 1 * time.Second
		timeout   = time.NewTimer(1 * time.Millisecond)
		deadline  = time.NewTimer(60 * time.Second)
	)

	defer timeout.Stop()
	defer deadline.Stop()

	for l.state.test(line_peering, 0) {
		select {

		case <-l.shutdown:
			l.state.mod(line_terminating, line_active)

		case <-deadline.C:
			l.state.mod(line_broken, line_active)

		case cmd := <-l.snd_open_chan:
			l.snd_open_pkt(cmd.netpath)

		case <-timeout.C:
			if l.snd_open_pkt(nil) != nil {
				l.sw.net.send_nat_breaker(l.peer)
			}
			timeout.Reset(timeout_d)

		case cmd := <-l.rcv_open_chan:
			l.state.mod(0, line_peering)
			l.rcv_open_chan <- cmd

		}
	}
}

// open procedure
func (l *line_t) run_open_loop() {
	var (
		timeout_d     = 1 * time.Second
		timeout       = time.NewTimer(1 * time.Millisecond)
		deadline      = time.NewTimer(60 * time.Second)
		send_open     bool
		received_open bool
	)

	defer timeout.Stop()
	defer deadline.Stop()

	for l.state.test(line_opening, 0) {
		select {

		case <-l.shutdown:
			l.state.mod(line_terminating, line_active)

		case <-deadline.C:
			l.handle_err(fmt.Errorf("line opend failed: deadline reached"))
			l.state.mod(line_broken, line_active)

		case cmd := <-l.snd_open_chan:
			l.snd_open_pkt(cmd.netpath)

		case <-timeout.C:
			if l.handle_err(l.snd_open_pkt(nil)) {
				send_open = true
			}
			timeout.Reset(timeout_d)

		case cmd := <-l.rcv_open_chan:
			if l.handle_err(l.rcv_open_pkt(cmd)) {
				received_open = true
			} else {
				l.state.mod(0, line_active)
			}

		}

		if send_open && received_open {
			l.state.mod(line_opened, line_active)
		}
	}
}

func (l *line_t) run_line_loop() {
	var (
		local_hashname = l.sw.hashname
		broken_timeout = 60 * time.Second
		broken_timer   = time.NewTimer(broken_timeout)
		broken_chan    = make(chan time.Time, 1)
		idle_timeout   = 55 * time.Second
		idle_timer     = time.NewTimer(idle_timeout)
		ack_ticker     = time.NewTicker(10 * time.Millisecond)
		seek_d         = 30 * time.Second
		seek_timer     = time.NewTimer(1 * time.Millisecond)
		ping_d         = 1 * time.Second
		ping_timer     = time.NewTimer(ping_d)
	)

	defer broken_timer.Stop()
	defer idle_timer.Stop()
	defer ack_ticker.Stop()
	defer seek_timer.Stop()
	defer ping_timer.Stop()
	defer close(broken_chan)

	l.open_retries = 3

	l.activate()
	defer l.deactivate()

	l.log.Noticef("line opened: id=%s:%s",
		short_hash(l.prv_key.id),
		short_hash(l.pub_key.id))

	time.Sleep(100 * time.Millisecond)

	for _, c := range l.channels {
		c.cnd.Broadcast()
	}

	defer func() {
		for _, c := range l.channels {
			c.cnd.Broadcast()
		}
	}()

	for l.state.test(line_opened, 0) {
		select {

		case <-l.shutdown:
			l.state.mod(line_terminating, line_active)

		case <-broken_timer.C:
			l.state.mod(line_broken, line_active)

		case <-broken_chan:
			l.state.mod(line_broken, line_active)

		case <-idle_timer.C:
			l.state.mod(line_idle, line_active)

		case now := <-ack_ticker.C:
			for _, c := range l.channels {
				ack, miss := c.tick(now)
				if ack != nil {
					l.snd_line_pkt(cmd_line_snd{ack, nil})
				}
				for _, pkt := range miss {
					l.snd_line_pkt(cmd_line_snd{pkt, nil})
				}
			}
			for _, c := range l.channels {
				if c.is_closed() {
					c.log.Debugf("channel[%s:%s](%s -> %s): closed",
						short_hash(c.channel_id),
						c.channel_type,
						l.sw.hashname.Short(),
						l.peer.Hashname().Short())
					delete(l.channels, c.channel_id)
				}
			}

		case <-seek_timer.C:
			seek_timer.Reset(seek_d)
			go l.snd_seek(broken_chan, local_hashname)

		case <-ping_timer.C:
			go l.snd_ping(broken_chan, ping_timer)

		case cmd := <-l.snd_chan:
			if l.handle_err(l.snd_line_pkt(cmd)) {
				idle_timer.Reset(idle_timeout)
			} else {
				l.state.mod(0, line_active) // connection is broken
			}

		case pkt := <-l.rcv_line_chan:
			if l.handle_err(l.rcv_line_pkt(pkt)) {
				idle_timer.Reset(idle_timeout)
				broken_timer.Reset(broken_timeout)
			}

		case channel := <-l.add_channel_chan:
			l.channels[channel.channel_id] = channel
			channel.cnd.Broadcast()

		case cmd := <-l.snd_open_chan:
			l.snd_open_pkt(cmd.netpath)

		case cmd := <-l.rcv_open_chan:
			l.rcv_open_pkt(cmd)

		}
	}
}

func (l *line_t) snd_seek(broken_chan chan<- time.Time, local_hashname Hashname) {
	for i := 0; i < 3; i++ {
		err := l.sw.seek_handler.Seek(l.peer.Hashname(), local_hashname)
		if err == nil {
			return
		}
		l.log.Noticef("seeking failed: err=%s", err)
		time.Sleep(1 * time.Second)
	}

	func() {
		defer func() { recover() }()
		broken_chan <- time.Now()
		l.log.Noticef("seeking failed (breaking the line)")
	}()
}

func (l *line_t) snd_ping(broken_chan chan<- time.Time, ping_timer *time.Timer) {
	// for i := 0; i < 3; i++ {
	//   if l.sw.ping_handler.Ping(l.peer.Hashname()) {
	//     ping_timer.Reset(1 * time.Second)
	//     return
	//   }
	// }

	// func() {
	//   defer func() { recover() }()
	//   broken_chan <- time.Now()
	//   l.log.Noticef("ping failed (breaking the line)")
	// }()
}

func (l *line_t) rcv_line_pkt(opkt *pkt_t) error {
	ipkt, err := l.shr_key.dec(opkt)
	if err != nil {
		return err
	}

	ipkt.peer = l.peer
	ipkt.netpath = opkt.netpath

	if ipkt.hdr.C == "" {
		return errInvalidPkt
	}

	// send pkt to existing channel
	if channel := l.channels[ipkt.hdr.C]; channel != nil {
		l.peer.AddNetPath(ipkt.netpath, true)
		l.log.Debugf("rcv pkt: addr=%s hdr=%+v", l.peer, ipkt.hdr)
		return channel.push_rcv_pkt(ipkt)
	}

	// open new channel
	if ipkt.hdr.Type == "" {
		return errInvalidPkt
	}

	raw := !ipkt.hdr.Seq.IsSet()

	if !raw && ipkt.hdr.Seq.Get() != 0 {
		return errInvalidPkt
	}

	channel, err := make_channel(l.sw, l, ipkt.hdr.C, ipkt.hdr.Type, false, raw)
	if err != nil {
		return err
	}

	l.channels[channel.channel_id] = channel

	l.log.Debugf("rcv pkt: addr=%s hdr=%+v", l.peer, ipkt.hdr)

	channel.log.Debugf("channel[%s:%s](%s -> %s): opened",
		short_hash(channel.channel_id),
		ipkt.hdr.Type,
		l.sw.hashname.Short(),
		l.peer.Hashname().Short())

	err = channel.push_rcv_pkt(ipkt)
	if err != nil {
		return err
	}

	l.peer.AddNetPath(ipkt.netpath, true)
	go channel.run_user_handler()

	return nil
}

func (l *line_t) snd_line_pkt(cmd cmd_line_snd) error {
	pkt, err := l.shr_key.enc(cmd.pkt)
	if err != nil {
		if cmd.reply != nil {
			cmd.reply <- err
		}
		return err
	}

	err = l.peer.ActivePath().Send(l.sw, pkt)
	if err != nil {
		if cmd.reply != nil {
			cmd.reply <- err
		}
		return err
	}

	if cmd.reply != nil {
		cmd.reply <- nil
	}
	return nil
}

func (l *line_t) rcv_open_pkt(cmd cmd_open_rcv) error {
	var (
		err            error
		pub            = cmd.pub
		netpath        = cmd.netpath
		local_rsa_key  = l.sw.key
		local_hashname = l.sw.hashname
	)

	prv := l.prv_key
	if prv == nil {
		prv, err = make_line_half(local_rsa_key, pub.rsa_pubkey)
		if err != nil {
			l.log.Noticef("rcv open from=%s err=%s", netpath, err)
			return err
		}
	}

	err = pub.verify(l.pub_key, local_hashname)
	if err != nil {
		l.log.Noticef("rcv open from=%s err=%s", netpath, err)
		return err
	}

	shr, err := line_activate(prv, pub)
	if err != nil {
		l.log.Noticef("rcv open from=%s err=%s", netpath, err)
		return err
	}

	l.peer.SetPublicKey(pub.rsa_pubkey)
	l.peer.AddNetPath(netpath, true)

	l.log.Debugf("rcv open from=%s", l.peer)

	l.prv_key = prv
	l.pub_key = pub
	l.shr_key = shr

	return nil
}

func (l *line_t) snd_open_pkt(np NetPath) error {
	var (
		local_rsa_key = l.sw.key
		netpaths      = l.peer.NetPaths()
	)

	if np != nil {
		netpaths = []NetPath{np}
	}

	if l.peer.Hashname().IsZero() {
		l.log.Debugf("snd open to=%s err=%s", l.peer, errInvalidOpenReq)
		return errInvalidOpenReq
	}

	if len(netpaths) == 0 {
		l.log.Debugf("snd open to=%s err=%s", l.peer, errInvalidOpenReq)
		return errInvalidOpenReq
	}

	if l.peer.PublicKey() == nil {
		l.log.Debugf("snd open to=%s err=%s", l.peer, errMissingPublicKey)
		return errMissingPublicKey
	}

	if l.prv_key == nil {
		prv_key, err := make_line_half(local_rsa_key, l.peer.PublicKey())
		if err != nil {
			l.log.Debugf("snd open to=%s err=%s", l.peer, err)
			return err
		}
		l.prv_key = prv_key
	}

	for _, np := range netpaths {
		pkt, err := l.prv_key.compose_open_pkt()
		pkt.netpath = np
		pkt.peer = l.peer

		err = np.Send(l.sw, pkt)
		if err != nil {
			l.log.Debugf("snd open to=%s err=%s", l.peer, err)
		} else {
			l.log.Debugf("snd open to=%s", l.peer)
		}
	}

	return nil
}

func (l *line_t) teardown() {
	l.unregister()

	l.state.mod(0, line_active)

	l.break_channels()

	l.flush() // empty the buffers

	l.state.mod(0, line_running|line_terminating)

	if l.state.test(line_broken, 0) {
		l.log.Noticef("line closed: peer=%s (reason=%s)",
			l.peer.String(),
			"broken")
	} else if l.state.test(line_idle, 0) {
		l.log.Noticef("line closed: peer=%s (reason=%s)",
			l.peer.String(),
			"idle")
	}
}

func (l *line_t) flush() {
	for {
		select {
		case <-l.snd_open_chan:
		case <-l.rcv_open_chan:
		case <-l.rcv_line_chan:
		case <-l.shutdown:
		case <-l.add_channel_chan:
		default:
			return
		}
	}
}

func (l *line_t) break_channels() {
	for _, c := range l.channels {
		c.mark_as_broken()

		c.log.Debugf("channel[%s:%s](%s -> %s): broken",
			short_hash(c.channel_id),
			c.channel_type,
			l.sw.hashname.Short(),
			l.peer.Hashname().Short())
	}

	// flush channel sends
	for {
		select {
		case cmd := <-l.snd_chan:
			cmd.reply <- ErrPeerBroken
		default:
			return
		}
	}
}

func (l *line_t) register() {
	l.sw.main.register_line_chan <- l
}

func (l *line_t) unregister() {
	l.sw.main.unregister_line_chan <- l
}

func (l *line_t) activate() {
	l.sw.main.activate_line_chan <- l
}

func (l *line_t) deactivate() {
	l.sw.main.deactivate_line_chan <- l
}

func (l *line_t) handle_err(err error) bool {
	if err != nil {
		l.err = err
		l.state.mod(line_error, 0)
		return false
	}
	return true
}
