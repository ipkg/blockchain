package blockchain

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	chord "github.com/euforia/go-chord"
	"github.com/ipkg/go-mux"
	"github.com/zeebo/bencode"
)

const (
	reqTypeFirstBlock byte = iota + 43
	reqTypeLastBlock
	reqTypeBlock
	reqTypeBlockBroadcast
	reqTypeTxBroadcast
)

type bcHeader struct {
	T byte
}

// ChordTransport for the blockchain
type ChordTransport struct {
	// this allows to re-use the listener for multiple rpc services
	sock *mux.Layer

	dialTimeout time.Duration

	cc   *chord.Config
	ring *chord.Ring

	olock sync.Mutex
	// outbound connections
	outbound map[string][]net.Conn

	ilock sync.RWMutex
	// inbound connections
	inbound map[net.Conn]bool

	// channel to send blocks from network
	bch chan<- Block
	// channel to send tx from network
	tch chan<- *Tx

	store BlockStore
}

// NewChordTransport initializes a new chord based transport for the blockchain.  The chord config
// is used to determine the braodcast spread and identifying self using the advertise address.
func NewChordTransport(sock *mux.Layer, cfg *chord.Config, ring *chord.Ring) *ChordTransport {
	ct := &ChordTransport{
		sock:        sock,
		dialTimeout: 5 * time.Second,
		cc:          cfg,
		ring:        ring,
		outbound:    map[string][]net.Conn{},
		inbound:     map[net.Conn]bool{},
	}

	return ct
}

// Initialize is called by the blockchain with the tx and block queues.  These are
// used when blocks and txs are received over the network to submit to the engine
// for processing.
func (ct *ChordTransport) Initialize(tx chan<- *Tx, blk chan<- Block, store BlockStore) error {
	ct.bch = blk
	ct.tch = tx
	ct.store = store

	go ct.listen()

	return nil
}

// BroadcastBlock to the network
func (ct *ChordTransport) BroadcastBlock(blk *Block) error {
	return ct.broadcast(reqTypeBlockBroadcast, blk.Hash(), blk)
}

// BroadcastTransaction to the network
func (ct *ChordTransport) BroadcastTransaction(tx *Tx) error {
	return ct.broadcast(reqTypeTxBroadcast, tx.Hash(), tx)
}

// LastBlock of the chain per the given host
func (ct *ChordTransport) LastBlock(host string) (*Block, error) {
	return ct.getBlockByType(reqTypeLastBlock, host)
}

// FirstBlock request from the given host.
func (ct *ChordTransport) FirstBlock(host string) (*Block, error) {
	return ct.getBlockByType(reqTypeFirstBlock, host)
}

// last or genesis block request
func (ct *ChordTransport) getBlockByType(typ byte, host string) (*Block, error) {
	conn, err := ct.getConn(host)
	if err != nil {
		return nil, err
	}

	var blk Block

	enc := bencode.NewEncoder(conn)
	if err = enc.Encode(&bcHeader{T: typ}); err == nil {
		dec := bencode.NewDecoder(conn)
		err = dec.Decode(&blk)
	}

	if err != nil {
		if err == io.EOF {
			err = nil
		}
		// don't return conn there is an error.  since we are using udp underneath, it
		// shouldn't be too expensive to get a new connection.
		conn.Close()
		return &blk, err
	}

	ct.returnConn(conn)
	return &blk, nil
}

func (ct *ChordTransport) getConn(addr string) (net.Conn, error) {
	var out net.Conn

	ct.olock.Lock()

	c, ok := ct.outbound[addr]
	if ok && len(c) > 0 {
		out = c[0]
		ct.outbound[addr] = c[1:]
	}
	ct.olock.Unlock()

	if out != nil {
		return out, nil
	}

	return ct.sock.Dial(addr, ct.dialTimeout)
}

func (ct *ChordTransport) returnConn(conn net.Conn) {
	addr := conn.RemoteAddr().String()

	ct.olock.Lock()
	defer ct.olock.Unlock()

	p, ok := ct.outbound[addr]
	if !ok {
		ct.outbound[addr] = []net.Conn{conn}
		return
	}
	ct.outbound[addr] = append(p, conn)
}

func (ct *ChordTransport) broadcast(typ byte, hsh []byte, v interface{}) error {
	nodes, err := ct.ring.Lookup(ct.cc.NumSuccessors, hsh)
	if err != nil {
		return err
	}

	go func(vns []*chord.Vnode) {
		hosts := VnodeSlice(vns).UniqueHosts()
		for _, host := range hosts {
			// skip self
			if host == ct.ring.Hostname() {
				continue
			}

			if err := ct.doRequest(host, &bcHeader{T: typ}, v, nil); err != nil {
				log.Println("ERR", err)
			}
		}

	}(nodes)
	return nil
}

func (ct *ChordTransport) doRequest(host string, hdr *bcHeader, req, resp interface{}) error {
	conn, err := ct.getConn(host)
	if err != nil {
		return err
	}

	enc := bencode.NewEncoder(conn)
	if err = enc.Encode(hdr); err == nil {
		if err = enc.Encode(req); err == nil {
			// optional response param
			if resp != nil {
				dec := bencode.NewDecoder(conn)
				err = dec.Decode(resp)
			}
		}
	}

	if err != nil {
		if err == io.EOF {
			err = nil
		}
		// Don't return conn there is an error.  since we are using udp underneath, it
		// shouldn't be too expensive to get a new connection.
		conn.Close()
		return err
	}

	ct.returnConn(conn)
	return nil
}

// RequestBlocks from the network.  The received blocks are sent to the block
// channel
func (ct *ChordTransport) RequestBlocks(hashes ...[]byte) {

	for _, hsh := range hashes {
		vns, err := ct.ring.Lookup(ct.cc.NumSuccessors, hsh)
		if err != nil {
			log.Println("ERR", err)
			continue
		}

		uhosts := VnodeSlice(vns).UniqueHosts()
		for _, host := range uhosts {
			if host == ct.ring.Hostname() {
				continue
			}

			var blk Block
			if e := ct.doRequest(host, &bcHeader{T: reqTypeBlock}, hsh, &blk); e != nil {
				log.Println("ERR", e)
				continue
			}

			if blk.BlockHeader == nil {
				continue
			}

			ct.bch <- blk
		}
	}
}

func (ct *ChordTransport) listen() {

	for {
		conn, err := ct.sock.Accept()
		if err != nil {
			log.Println("ERR", err)
			continue
		}

		ct.ilock.Lock()
		ct.inbound[conn] = true
		ct.ilock.Unlock()

		go ct.handleConn(conn)
	}
}

func (ct *ChordTransport) handleConn(conn net.Conn) {

	defer func() {
		ct.ilock.Lock()
		delete(ct.inbound, conn)
		ct.ilock.Unlock()
		conn.Close()
	}()

	enc := bencode.NewEncoder(conn)
	dec := bencode.NewDecoder(conn)

	for {

		var header bcHeader
		err := dec.Decode(&header)
		if err != nil {
			log.Println("WRN", err)
			return
		}

		switch header.T {
		case reqTypeBlock:
			var h []byte
			if err = dec.Decode(&h); err != nil {
				break
			}

			b := ct.store.Get(h)
			if b == nil {
				// signifies we don't have the block.
				b = &Block{}
			}

			err = enc.Encode(b)

		case reqTypeLastBlock:
			blk := ct.store.LastBlock()
			err = enc.Encode(blk)

		case reqTypeFirstBlock:
			blk := ct.store.FirstBlock()
			err = enc.Encode(blk)

		case reqTypeTxBroadcast:
			var tx Tx
			if err = dec.Decode(&tx); err == nil {
				ct.tch <- &tx
			}

		case reqTypeBlockBroadcast:
			var blk Block
			if err = dec.Decode(&blk); err == nil {
				ct.bch <- blk
			}

		default:
			err = fmt.Errorf("unknown request type: %d", header.T)
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("ERR %v", err)
			}

			// exit out of loop
			break
		}

	}

}

// Shutdown listener and all inbound and outbound connections.
func (ct *ChordTransport) Shutdown() {
	ct.sock.Close()

	// Close all the inbound connections
	ct.ilock.RLock()
	for conn := range ct.inbound {
		conn.Close()
	}
	ct.ilock.RUnlock()

	// Close all the outbound
	ct.olock.Lock()
	for _, conns := range ct.outbound {
		for _, out := range conns {
			out.Close()
		}
	}
	ct.outbound = nil
	ct.olock.Unlock()
}

// VnodeSlice allows operations against a set of vnodes
type VnodeSlice []*chord.Vnode

// UniqueHosts from a list of vnodes
func (vl VnodeSlice) UniqueHosts() []string {
	m := map[string]bool{}
	for _, v := range vl {
		m[v.Host] = true
	}
	out := make([]string, len(m))
	i := 0
	for k := range m {
		out[i] = k
		i++
	}
	return out
}

// VnodesByHost returns a map of vnodes to hosts.
func (vl VnodeSlice) VnodesByHost() map[string]VnodeSlice {
	m := map[string]VnodeSlice{}
	for _, vn := range vl {
		v, ok := m[vn.Host]
		if !ok {
			m[vn.Host] = VnodeSlice{vn}
			continue
		}
		v = append(v, vn)
		m[vn.Host] = v
	}
	return m
}