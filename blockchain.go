package blockchain

import (
	//"bytes"
	"log"
	"reflect"
	"time"
)

type Blockchain struct {
	CurrentBlock Block
	// Holds all available blocks
	BlockSlice
	// Channel to read incoming transactions from
	tq chan *Transaction
	// Channel on which generated blocks will be available
	bq chan Block
	// Channel to broadcast transaction and block messages to the network
	nq chan Message
	// Public-Private keypair for this chain
	Keypair *Keypair
}

func NewBlockchain(keypair *Keypair, broadcastChan chan Message) *Blockchain {
	bl := &Blockchain{
		tq:      make(chan *Transaction),
		bq:      make(chan Block),
		Keypair: keypair,
		nq:      broadcastChan,
	}
	// TODO: Read blockchain from file and stuff...
	bl.CurrentBlock = bl.CreateNewBlock()

	return bl
}

func (bl *Blockchain) QueueTransaction(tx *Transaction) {
	bl.tq <- tx
}

func (bl *Blockchain) QueueBlock(b Block) {
	bl.bq <- b
}

func (bl *Blockchain) CreateNewBlock() Block {

	prevBlock := bl.BlockSlice.PreviousBlock()
	prevBlockHash := []byte{}
	if prevBlock != nil {
		prevBlockHash = prevBlock.Hash()
	}

	b := NewBlock(prevBlockHash)
	b.BlockHeader.Origin = bl.Keypair.Public

	return b
}

func (bl *Blockchain) AddBlock(b Block) {
	bl.BlockSlice = append(bl.BlockSlice, b)
}

func (bl *Blockchain) Run() {

	interruptBlockGen := bl.GenerateBlocks()
	for {
		select {
		case tr := <-bl.tq:
			if bl.CurrentBlock.TransactionSlice.Exists(*tr) {
				continue
			}
			if !tr.VerifyTransaction(TRANSACTION_POW) {
				log.Println("Transaction verfication failed:", tr)
				continue
			}

			bl.CurrentBlock.AddTransaction(tr)
			interruptBlockGen <- bl.CurrentBlock
			// Build transaction message
			mes := NewMessage(MESSAGE_SEND_TRANSACTION)
			mes.Data, _ = tr.MarshalBinary()
			// Broadcast transaction message to the network
			time.Sleep(300 * time.Millisecond)
			bl.nq <- *mes

		case b := <-bl.bq:
			if bl.BlockSlice.Exists(b) {
				log.Println("Block exists:", b.String())
				continue
			}
			if !b.VerifyBlock(BLOCK_POW) {
				log.Println("Block verification failed:", b.String())
				continue
			}

			if reflect.DeepEqual(b.PrevBlock, bl.CurrentBlock.Hash()) {
				// I'm missing some blocks in the middle. Request'em.
				log.Println("Missing blocks in between")
			} else {
				log.Println("New block:", b.String())
				transDiff := TransactionSlice{}
				if !reflect.DeepEqual(b.BlockHeader.MerkelRoot, bl.CurrentBlock.MerkelRoot) {
					// Transactions are different
					log.Println("Transactions are different. Finding diff")
					transDiff = DiffTransactionSlices(*bl.CurrentBlock.TransactionSlice, *b.TransactionSlice)
				}

				bl.AddBlock(b)
				log.Println("Chain size:", len(bl.BlockSlice))

				//Broadcast block to network
				mes := NewMessage(MESSAGE_SEND_BLOCK)
				mes.Data, _ = b.MarshalBinary()
				bl.nq <- *mes
				//New Block
				bl.CurrentBlock = bl.CreateNewBlock()
				bl.CurrentBlock.TransactionSlice = &transDiff

				interruptBlockGen <- bl.CurrentBlock
			}
		}
	}
}

func (bl *Blockchain) GenerateBlocks() chan Block {
	interrupt := make(chan Block)

	go func() {
		block := <-interrupt

	loop:
		log.Println("Starting Proof of Work:", block.String())
		block.BlockHeader.MerkelRoot = block.GenerateMerkelRoot()
		block.BlockHeader.Nonce = 0
		block.BlockHeader.Timestamp = uint32(time.Now().Unix())

		for {

			sleepTime := time.Nanosecond
			if block.TransactionSlice.Len() > 0 {

				if CheckProofOfWork(BLOCK_POW, block.Hash()) {
					block.Signature = block.Sign(bl.Keypair)
					bl.bq <- block
					sleepTime = time.Hour * 24
					log.Println("Found Block:", block.String())
				} else {
					block.BlockHeader.Nonce += 1
				}

			} else {
				sleepTime = time.Hour * 24
				log.Println("No transactions. Sleeping for", sleepTime.Seconds(), "secs")
			}

			select {
			case block = <-interrupt:
				goto loop

			case <-time.After(sleepTime):
				continue
			}
		}
	}()

	return interrupt
}

//Assumes transaction arrays are sorted (which maybe is too big of an assumption)
func DiffTransactionSlices(a, b TransactionSlice) (diff TransactionSlice) {
	lastj := 0
	for _, t := range a {
		found := false
		for j := lastj; j < len(b); j++ {
			if reflect.DeepEqual(b[j].Signature, t.Signature) {
				found = true
				lastj = j
				break
			}
		}

		if !found {
			diff = append(diff, t)
		}
	}

	return
}
