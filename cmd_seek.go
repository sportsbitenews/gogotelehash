package telehash

import (
	"net"
	"strings"
	"sync"
	"time"
)

func (s *Switch) Seek(hashname string, n int) []string {
	var (
		wg   sync.WaitGroup
		last = s.find_closest_hashnames(hashname, n)
	)

RECURSOR:
	for {
		for _, to := range last {
			wg.Add(1)
			go s.send_seek_cmd(to, hashname, &wg)
		}

		wg.Wait()

		curr := s.find_closest_hashnames(hashname, n)
		if len(curr) != len(last) {
			last = curr
			continue RECURSOR
		}

		for i, a := range last {
			if a != curr[i] {
				last = curr
				continue RECURSOR
			}
		}

		break
	}

	return last
}

func (s *Switch) send_seek_cmd(to, seek string, wg *sync.WaitGroup) {
	defer wg.Done()

	pkt := &pkt_t{
		hdr: pkt_hdr_t{
			Type: "seek",
			Seek: seek,
		},
	}

	channel, err := s.open_channel(to, pkt)
	if err != nil {
		Log.Debugf("failed to seek %s (error: %s)", to, err)
		return
	}
	defer channel.Close()

	channel.SetReceiveDeadline(time.Now().Add(15 * time.Second))

	reply, err := channel.receive()
	if err != nil {
		Log.Debugf("failed to seek %s (error: %s)", to, err)
		return
	}

	for _, rec := range reply.hdr.See {
		fields := strings.Split(rec, ",")

		if len(fields) != 3 {
			continue
		}

		var (
			hashname = fields[0]
			ip       = fields[1]
			port     = fields[2]
			addr     = net.JoinHostPort(ip, port)
			udpaddr  *net.UDPAddr
			err      error
		)

		udpaddr, err = net.ResolveUDPAddr("udp", addr)
		if err != nil {
			continue
		}

		s.c_queue <- &cmd_peer_register{
			peer: make_peer(s, hashname, udpaddr, nil),
		}
	}
}