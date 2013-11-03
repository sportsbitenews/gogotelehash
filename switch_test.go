package telehash

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"github.com/fd/go-util/log"
	"runtime"
	"testing"
	"time"
)

func init() {
	Log.SetLevel(log.DEBUG)
}

func TestOpen(t *testing.T) {
	runtime.GOMAXPROCS(runtime.NumCPU())

	var (
		key_a = make_key()
		a     = make_switch("127.0.0.1:4000", key_a)

		key_b = make_key()
		b     = make_switch("127.0.0.1:4001", key_b)
	)

	go a.Run()
	go b.Run()
	defer a.Close()
	defer b.Close()

	go func() {

		hashname, err := a.RegisterPeer("127.0.0.1:4001", &key_b.PublicKey)
		if err != nil {
			t.Fatal(err)
		}

		channel, err := a.Open(hashname, "_greetings")
		if err != nil {
			t.Fatal(err)
		}

		defer channel.Close()

		for i := 0; i < 1000; i++ {
			channel.Send(nil, []byte(fmt.Sprintf("hello world (%d)", i)))
		}
	}()

	go func() {

		channel := b.Accept()
		defer channel.Close()

		msg, err := channel.Receive(nil)
		if err != nil {
			t.Fatal(err)
		}
		Log.Infof("msg=%q", msg)

		for i := 0; i < 1000; i++ {
			msg, err = channel.Receive(nil)
			if err != nil {
				t.Fatal(err)
			}

			Log.Infof("msg=%q", msg)
		}
	}()

	time.Sleep(5 * time.Second)

	if a.err != nil {
		t.Fatal(a.err)
	}
	if b.err != nil {
		t.Fatal(b.err)
	}
}

func make_key() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

func make_switch(addr string, key *rsa.PrivateKey) *Switch {
	s, err := NewSwitch(addr, key)
	if err != nil {
		panic(err)
	}
	return s
}