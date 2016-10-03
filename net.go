package blockchain

import (
	_ "fmt"
	"io"
	"log"
	"net"
	"time"
)

const (
	BLOCKCHAIN_DEFAULT_PORT int = 9119
	MAX_NODE_CONNECTIONS        = 400

	NETWORK_KEY_SIZE = 80

	TRANSACTION_HEADER_SIZE = NETWORK_KEY_SIZE /* from key */ + NETWORK_KEY_SIZE /* to key */ + 4 /* int32 timestamp */ + 32 /* sha256 payload hash */ + 4 /* int32 payload length */ + 4 /* int32 nonce */

	BLOCK_HEADER_SIZE = NETWORK_KEY_SIZE /* origin key */ + 4 /* int32 timestamp */ + 32 /* prev block hash */ + 32 /* merkel tree hash */ + 4 /* int32 nonce */

	KEY_SIZE = 28
)

type ConnectionsQueue chan string

type Network struct {
	Nodes                     // map of connected nodes
	Address            string // bind address
	ConnectionsQueue          // nodes to connect to
	ConnectionCallback NodeChannel
	BroadcastQueue     chan Message
	IncomingMessages   chan Message
}

func NewNetwork(addr string) *Network {
	n := &Network{
		BroadcastQueue:     make(chan Message),
		IncomingMessages:   make(chan Message),
		ConnectionsQueue:   make(ConnectionsQueue),
		ConnectionCallback: make(NodeChannel),
		Address:            addr,
		Nodes:              Nodes{},
	}
	return n
}

func (n *Network) Run() error {
	go n.watchConnQueue()

	listenCb, err := n.startListening()
	if err != nil {
		return err
	}

	for {
		select {
		case node := <-listenCb:
			n.Nodes.AddNode(node)

		case node := <-n.ConnectionCallback:
			n.Nodes.AddNode(node)

		case message := <-n.BroadcastQueue:
			go n.BroadcastMessage(message)
		}
	}
	return nil
}

func (n *Network) watchConnQueue() {
	for {
		address := <-n.ConnectionsQueue
		if address != n.Address && n.Nodes[address] == nil {
			go func() {
				if err := dialNode(address, 5*time.Second, false, n.ConnectionCallback, n.IncomingMessages); err != nil {
					log.Println("ERR dialNode", err)
				}
			}()
		}
	}
}

func (n *Network) startListening() (NodeChannel, error) {
	listener, err := getListener(n.Address)
	if err != nil {
		return nil, err
	}

	cb := make(NodeChannel)
	go func(l *net.TCPListener, inMsg chan Message) {
		for {
			conn, err := l.AcceptTCP()
			if err != nil {
				if err != io.EOF {
					log.Println("ERR", err)
				}
				continue
			}

			log.Println("Connecting", conn.RemoteAddr().String())
			nd := NewNode(conn, inMsg)
			cb <- nd
		}
	}(listener, n.IncomingMessages)

	log.Println("Listening on:", n.Address)
	return cb, nil
}

func (n *Network) BroadcastMessage(message Message) {
	b, _ := message.MarshalBinary()

	for k, node := range n.Nodes {
		log.Println("Broadcasting:", k)
		go func() {
			if _, err := node.TCPConn.Write(b); err != nil {
				log.Println("Error Broadcasting to", node.TCPConn.RemoteAddr())
			}
		}()
	}
}

func dialNode(dst string, timeout time.Duration, retry bool, cb NodeChannel, inMsg chan Message) error {
	// bail if bad address
	addrDst, err := net.ResolveTCPAddr("tcp4", dst)
	if err != nil {
		return err
	}
	var con *net.TCPConn = nil

loop:
	for {

		breakChannel := make(chan bool)
		go func() {
			if con, err = net.DialTCP("tcp", nil, addrDst); con != nil {
				n := NewNode(con, inMsg)
				cb <- n
				breakChannel <- true
			}
		}()

		select {
		case <-time.After(timeout):
			if !retry {
				break loop
			}

		case <-breakChannel:
			break loop

		}

	}

	return nil
}

func getListener(addr string) (*net.TCPListener, error) {
	a, err := net.ResolveTCPAddr("tcp4", addr)
	if err == nil {
		return net.ListenTCP("tcp4", a)
	}
	return nil, err
}

/*
func GetIpAddress() []string {
	if name, err := os.Hostname(); err == nil {
		if addrs, err := net.LookupHost(name); err == nil {
			return addrs
		}
	}
	return nil
}
*/
func networkError(err error) {
	if err != nil && err != io.EOF {
		log.Println("[ERR] Blockchain network:", err)
	}
}
