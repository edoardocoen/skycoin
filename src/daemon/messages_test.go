package daemon

import (
    "bytes"
    "errors"
    "fmt"
    "github.com/op/go-logging"
    "github.com/skycoin/gnet"
    "github.com/skycoin/pex"
    "github.com/stretchr/testify/assert"
    "io/ioutil"
    "net"
    "testing"
    "time"
)

var (
    poolPort             = 6688
    addrIP               = "111.22.33.44"
    addrbIP              = "111.33.44.55"
    addrPort      uint16 = 5555
    addrbPort     uint16 = 6666
    addr                 = "111.22.33.44:5555"
    addrb                = "112.33.44.55:6666"
    addrc                = "112.22.33.55:4343"
    badAddrPort          = "111.22.44.33:x"
    badAddrNoPort        = "111.22.44.33"
    silenceLogger        = false
)

func init() {
    if silenceLogger {
        logging.SetBackend(logging.NewLogBackend(ioutil.Discard, "", 0))
    }
}

func TestRegisterMessages(t *testing.T) {
    gnet.EraseMessages()
    c := NewMessagesConfig()
    assert.NotPanics(t, c.Register)
    gnet.EraseMessages()
}

func TestNewIPAddr(t *testing.T) {
    i, err := NewIPAddr(addr)
    assert.Nil(t, err)
    assert.Equal(t, i.IP, uint32(1863721260))
    assert.Equal(t, i.Port, uint16(5555))

    bad := []string{"", "127.0.0.1", "127.0.0.1:x", "x:7777", ":",
        "127.0.0.1:7777:7777", "2001:0db8:85a3:0000:0000:8a2e:0370:7334",
        "[1fff:0:a88:85a3::ac1f]:8001"}
    for _, b := range bad {
        _, err := NewIPAddr(b)
        assert.NotNil(t, err)
    }
}

func TestIPAddrString(t *testing.T) {
    i, err := NewIPAddr(addr)
    assert.Nil(t, err)
    assert.Equal(t, addr, i.String())
}

func testSimpleMessageHandler(t *testing.T, d *Daemon, m gnet.Message) {
    assert.Nil(t, m.Handle(messageContext(addr), d))
    assert.Equal(t, len(d.Messages.Events), 1)
    if len(d.Messages.Events) != 1 {
        t.Fatal("messageEvent is empty")
    }
    <-d.Messages.Events
}

func TestGetPeersMessage(t *testing.T) {
    d := newDefaultDaemon()
    m := NewGetPeersMessage()
    testSimpleMessageHandler(t, d, m)
    d.Peers.Peers.AddPeer(addr)
    m.c = messageContext(addr)
    assert.NotPanics(t, func() { m.Process(d) })
    assert.NotEqual(t, m.c.Conn.LastSent, time.Unix(0, 0))

    // If no peers, nothing should happen
    m.c.Conn.LastSent = time.Unix(0, 0)
    delete(d.Peers.Peers.Peerlist, addr)
    assert.NotPanics(t, func() { m.Process(d) })
    assert.Equal(t, m.c.Conn.LastSent, time.Unix(0, 0))

    // Test with failing send
    d.Peers.Peers.AddPeer(addr)
    m.c.Conn.Conn = NewFailingConn(addr)
    assert.NotPanics(t, func() { m.Process(d) })
    assert.Equal(t, m.c.Conn.LastSent, time.Unix(0, 0))

    gnet.EraseMessages()
    shutdown(d)
}

func TestGivePeersMessage(t *testing.T) {
    d := newDefaultDaemon()
    addrs := []string{addr, addrb, "7"}
    peers := make([]*pex.Peer, 0, 3)
    for _, addr := range addrs {
        peers = append(peers, &pex.Peer{Addr: addr})
    }
    m := NewGivePeersMessage(peers)
    assert.Equal(t, len(m.GetPeers()), 2)
    testSimpleMessageHandler(t, d, m)
    assert.Equal(t, m.GetPeers()[0], addrs[0])
    assert.Equal(t, m.GetPeers()[1], addrs[1])
    // Peers should be added to the pex when processed
    m.Process(d)
    assert.Equal(t, len(d.Peers.Peers.Peerlist), 2)
    gnet.EraseMessages()
    shutdown(d)
}

func TestIntroductionMessageHandle(t *testing.T) {
    d := newDefaultDaemon()
    mc := messageContext(addr)
    m := NewIntroductionMessage(d.Messages.Mirror, d.Config.Version,
        d.Pool.Pool.ListenPort)

    // Test valid handling
    m.Mirror = d.Messages.Mirror + 1
    err := m.Handle(mc, d)
    assert.Nil(t, err)
    if len(d.Messages.Events) == 0 {
        t.Fatalf("messageEvent is empty")
    }
    <-d.Messages.Events
    assert.True(t, m.valid)
    m.valid = false

    // Test matching mirror
    m.Mirror = d.Messages.Mirror
    err = m.Handle(mc, d)
    assert.Equal(t, err, DisconnectSelf)
    m.Mirror = d.Messages.Mirror + 1
    assert.False(t, m.valid)

    // Test mismatched d.Config.Version
    m.Version = d.Config.Version + 1
    err = m.Handle(mc, d)
    assert.Equal(t, err, DisconnectInvalidVersion)
    assert.False(t, m.valid)

    // Test already connected
    d.mirrorConnections[m.Mirror] = make(map[string]uint16)
    d.mirrorConnections[m.Mirror][addrIP] = addrPort + 1
    err = m.Handle(mc, d)
    assert.Equal(t, err, DisconnectConnectedTwice)
    delete(d.mirrorConnections, m.Mirror)
    assert.False(t, m.valid)

    for len(d.Messages.Events) > 0 {
        <-d.Messages.Events
    }
    gnet.EraseMessages()
}

func TestIntroductionMessageProcess(t *testing.T) {
    cleanupPeers()
    d := newDefaultDaemon()
    m := NewIntroductionMessage(d.Messages.Mirror, d.Config.Version,
        uint16(poolPort))
    m.c = messageContext(addr)
    d.Pool.Pool.Pool[1] = m.c.Conn

    // Test invalid
    m.valid = false
    d.expectingIntroductions[addr] = time.Now()
    m.Process(d)
    // d.expectingIntroductions should get updated
    _, x := d.expectingIntroductions[addr]
    assert.False(t, x)
    // d.mirrorConnections should not have an entry
    _, x = d.mirrorConnections[m.Mirror]
    assert.False(t, x)
    assert.Equal(t, len(d.Peers.Peers.Peerlist), 0)

    // Test valid
    m.valid = true
    d.expectingIntroductions[addr] = time.Now()
    m.Process(d)
    // d.expectingIntroductions should get updated
    _, x = d.expectingIntroductions[addr]
    assert.False(t, x)
    assert.Equal(t, len(d.Peers.Peers.Peerlist), 1)
    assert.Equal(t, d.connectionMirrors[addr], m.Mirror)
    assert.NotNil(t, d.mirrorConnections[m.Mirror])
    assert.Equal(t, d.mirrorConnections[m.Mirror][addrIP], addrPort)
    peerAddr := fmt.Sprintf("%s:%d", addrIP, poolPort)
    assert.NotNil(t, d.Peers.Peers.Peerlist[peerAddr])

    // Handle impossibly bad ip:port returned from conn.Addr()
    // User should be disconnected
    m.valid = true
    m.c = messageContext(badAddrPort)
    m.Process(d)
    if len(d.Pool.Pool.DisconnectQueue) != 1 {
        t.Fatalf("DisconnectQueue empty")
    }
    <-d.Pool.Pool.DisconnectQueue

    m.valid = true
    m.c = messageContext(badAddrNoPort)
    m.Process(d)
    if len(d.Pool.Pool.DisconnectQueue) != 1 {
        t.Fatalf("DisconnectQueue empty")
    }
    <-d.Pool.Pool.DisconnectQueue

    gnet.EraseMessages()
}

func TestPingMessage(t *testing.T) {
    d := newDefaultDaemon()
    m := &PingMessage{}
    testSimpleMessageHandler(t, d, m)

    m.c = messageContext(addr)
    assert.NotPanics(t, func() { m.Process(d) })
    // A pong message should have been sent
    assert.NotEqual(t, m.c.Conn.LastSent, time.Unix(0, 0))

    // Failing to send should not cause a panic
    m.c.Conn.Conn = NewFailingConn(addr)
    m.c.Conn.LastSent = time.Unix(0, 0)
    assert.NotPanics(t, func() { m.Process(d) })
    assert.Equal(t, m.c.Conn.LastSent, time.Unix(0, 0))

    gnet.EraseMessages()
}

func TestPongMessage(t *testing.T) {
    cmsgs := NewMessagesConfig()
    cmsgs.Register()
    m := &PongMessage{}
    // Pongs dont do anything
    assert.Nil(t, m.Handle(messageContext(addr), nil))
    gnet.EraseMessages()
}

/* Helpers */

func gnetConnection(addr string) *gnet.Connection {
    return &gnet.Connection{
        Id:           1,
        Conn:         NewDummyConn(addr),
        LastSent:     time.Unix(0, 0),
        LastReceived: time.Unix(0, 0),
        Buffer:       &bytes.Buffer{},
    }
}

func messageContext(addr string) *gnet.MessageContext {
    return &gnet.MessageContext{
        Conn: gnetConnection(addr),
    }
}

type DummyGivePeersMessage struct {
    peers []*pex.Peer
}

func (self *DummyGivePeersMessage) Send(c net.Conn) error {
    return nil
}

func (self *DummyGivePeersMessage) GetPeers() []string {
    p := make([]string, 0, len(self.peers))
    for _, ps := range self.peers {
        p = append(p, ps.Addr)
    }
    return p
}

type DummyAddr struct {
    addr string
}

func NewDummyAddr(addr string) net.Addr {
    return &DummyAddr{addr: addr}
}

func (self *DummyAddr) String() string {
    return self.addr
}

func (self *DummyAddr) Network() string {
    return self.addr
}

type DummyConn struct {
    net.Conn
    addr string
}

func NewDummyConn(addr string) net.Conn {
    return &DummyConn{addr: addr}
}

func (self *DummyConn) RemoteAddr() net.Addr {
    return NewDummyAddr(self.addr)
}

func (self *DummyConn) LocalAddr() net.Addr {
    return self.RemoteAddr()
}

func (self *DummyConn) Close() error {
    return nil
}

func (self *DummyConn) Read(b []byte) (int, error) {
    return 0, nil
}

func (self *DummyConn) SetWriteDeadline(t time.Time) error {
    return nil
}

func (self *DummyConn) Write(b []byte) (int, error) {
    return len(b), nil
}

type FailingConn struct {
    DummyConn
}

func NewFailingConn(addr string) net.Conn {
    return &FailingConn{DummyConn{addr: addr}}
}

func (self *FailingConn) Write(b []byte) (int, error) {
    return 0, errors.New("failed")
}