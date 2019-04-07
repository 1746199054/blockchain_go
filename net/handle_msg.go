package net

import (
	"blockchain_go/blockchain"
	"blockchain_go/log"
	"blockchain_go/miner"
	"blockchain_go/transaction"
	"bytes"
	"encoding/hex"
	"time"
)

// 正在下载中的区块hash列表
var blocksInTransit = [][]byte{}
var mempool = make(map[string]transaction.Transaction)

/*
From https://en.bitcoin.it/wiki/Version_Handshake
## Version Handshake
When the local peer (L) connects to a remote peer (R), the remote peer will not send any data until it receives a version message.

L -> R: Send version message with the local peer's version
R -> L: Send version message back
R -> L: Send verack message
R:      Sets version to the minimum of the 2 versions
L -> R: Send verack message after receiving version message from R
L:      Sets version to the minimum of the 2 versions
本实现并不关心version消息的Version字段
*/

/*
version消息 "你好，我的区块高度是..."

发送条件：
完成节点发现后，就向所有对等节点发送version消息，告诉其他节点本节点的信息，请求与其他节点建立连接。
在节点完成交换version消息之前，节点不能与对方发送其他消息。

消息处理逻辑：
 - 若本节点是首先发出version消息的节点，在收到fromAddr的回应version消息后，需要向fromAddr发送verack消息
 - 若本节点在此之前没有向fromAddr发出version消息，即fromAddr请求与本节点建立连接，通过回应version消息和verack消息接受请求

See https://en.bitcoin.it/wiki/Version_Handshake
*/
func (payload *version) handleMsg(bc *blockchain.Blockchain, fromAddr string) {
	status, exist := connectingPeers[fromAddr]
	if exist {
		// 本节点是首先发出version消息的节点
		if status.status != waitVer {
			// TODO: 异常处理
			return
		}

		status.status = waitVerAck
		status.timestamp = time.Now().Unix()
		status.versionMsg = payload
		sendVerack(fromAddr)
	} else {
		if len(connectingPeers) >= maxConnectPeer {
			return
		}
		connectingPeers[fromAddr] = &connectingPeerStatus{waitVerAck, time.Now().Unix(), payload}
		sendVersion(fromAddr, bc)

		sendVerack(fromAddr)
	}

}

/*
verack消息 "同意连接"
用于回应收到的version消息

消息处理逻辑：
 - 若本节点的区块链的高度小于发送节点，说明本节点有未接收的区块，需要向对等节点获取区块。
 - 若本节点的区块链的高度大于发送节点，则向消息来源节点发送version消息，表明对方节点有未接收的区块。
*/
func (payload *verack) handleMsg(bc *blockchain.Blockchain, fromAddr string) {
	status, exist := connectingPeers[fromAddr]
	if !exist {
		// TODO: 异常处理
		return
	}
	if status.status != waitVerAck {
		// TODO: 异常处理
		return
	}

	delete(connectingPeers, fromAddr)
	activePeers[fromAddr] = time.Now().Unix()

	versionMsg := status.versionMsg
	myBestHeight := bc.GetBestHeight()
	foreignerBestHeight := versionMsg.BestHeight

	if myBestHeight < foreignerBestHeight {
		sendGetBlocks(fromAddr)
	}
}

/*
getblocks消息 "给我看看你有哪些区块"
用于获取对方节点的区块哈希列表。

发送条件：
当节点知道自己有未接收的区块但又不知道缺失区块的哈希时，要向对等节点请求节点的哈希列表。

消息处理逻辑：
使用`inv`消息向来源节点发送本节点的哈希列表。

TODO:
对方节点没有时，向其他节点获取或者退后区块获取
*/
func (payload *getblocks) handleMsg(bc *blockchain.Blockchain, fromAddr string) {
	blocks := bc.GetBlockHashes()
	sendInv(fromAddr, "block", blocks)
}

/*
inv消息 "我有这些区块/交易"
用来告诉其他节点本节点含有的区块链或交易的哈希。

发送条件：
 - 当节点发起交易或者挖出新区快时使用此消息向网络广播
 - 在接收到getblocks消息时使用`inv`消息进行回应。

消息处理逻辑：
比较本地有无相关区块或交易，没有则通过getdata消息获取相关数据。
*/
func (payload *inv) handleMsg(bc *blockchain.Blockchain, fromAddr string) {

	log.Net.Printf("Recevied inventory with %d %s\n", len(payload.Items), payload.Type)

	if payload.Type == "block" {
		blocksInTransit = payload.Items

		blockHash := payload.Items[0]
		sendGetData(fromAddr, "block", blockHash)

		newInTransit := [][]byte{}
		for _, b := range blocksInTransit {
			if bytes.Compare(b, blockHash) != 0 {
				newInTransit = append(newInTransit, b)
			}
		}
		blocksInTransit = newInTransit
	}

	if payload.Type == "tx" {
		txID := payload.Items[0]

		if mempool[hex.EncodeToString(txID)].ID == nil {
			sendGetData(fromAddr, "tx", txID)
		}
	}
}

/*
getdata消息 "给我发一下某区块/交易"
用于某个块或交易的请求

发送条件：
相应inv消息时，发现本地缺失某些交易/区块

消息处理逻辑：
消息处理逻辑：使用block或tx消息返回请求的块/交易
*/
func (payload *getdata) handleMsg(bc *blockchain.Blockchain, fromAddr string) {

	if payload.Type == "block" {
		block, err := bc.GetBlock([]byte(payload.ID))
		if err != nil {
			return
		}

		sendBlock(fromAddr, &block)
	}

	if payload.Type == "tx" {
		txID := hex.EncodeToString(payload.ID)
		tx := mempool[txID]

		SendTx(fromAddr, &tx)
		// delete(mempool, txID)
	}
}

/*
block消息 "给你区块数据"

发送条件：
用于对getdata消息进行相应，返回区块数据

消息处理逻辑：
验证区块，并将其放到本地区块链里

TODO：并非无条件信任，我们应该在将每个块加入到区块链之前对它们进行验证。
TODO: 并非运行 UTXOSet.Reindex()， 而是应该使用 UTXOSet.Update(block)，因为如果区块链很大，它将需要很多时间来对整个 UTXO 集重新索引。
*/
func (payload *block) handleMsg(bc *blockchain.Blockchain, fromAddr string) {
	blockData := payload.Block
	block := blockchain.DeserializeBlock(blockData)

	_, err := bc.GetBlock(block.Hash)
	if err == nil {
		return
	}

	log.Net.Println("Recevied a new block!")
	bc.AddBlock(block)

	log.Net.Printf("Added block %x\n", block.Hash)

	if len(blocksInTransit) > 0 {
		blockHash := blocksInTransit[0]
		sendGetData(fromAddr, "block", blockHash)

		blocksInTransit = blocksInTransit[1:]
	} else {
		UTXOSet := blockchain.UTXOSet{bc}
		UTXOSet.Reindex()
	}
}

/*
tx消息 "给你交易数据"

发送条件：
用于对getdata消息进行相应，返回交易数据

消息处理逻辑：
1. 对交易进行验证，将新交易放到内存池中
2. 向其他节点relay inv消息
https://en.bitcoin.it/wiki/Protocol_rules#.22tx.22_messages

TODO: 在将交易放到内存池之前，对其进行验证
TODO: orphan transactions 管理
*/
func (payload *tx) handleMsg(bc *blockchain.Blockchain, fromAddr string) {
	tx := payload.Transaction
	if tx.IsCoinbase() {
		// TODO 异常
		return
	}

	if len(tx.Vout) == 0 || len(tx.Vout) == 0 {
		return
	}

	_, exist := mempool[hex.EncodeToString(tx.ID)]
	if exist {
		return
	}

	mempool[hex.EncodeToString(tx.ID)] = tx

	if nodeAddress == knownNodes[0] {
		// 中心节点向其他节点广播交易消息
		for _, node := range knownNodes {
			if node != fromAddr {
				sendInv(node, "tx", [][]byte{tx.ID})
			}
		}
	} else {
		// 矿工节使用交易挖矿
		if len(mempool) >= 2 && len(miningAddress) > 0 {
		MineTransactions:
			var txs []*transaction.Transaction

			for id := range mempool {
				tx := mempool[id]
				if bc.VerifyTransactionSig(&tx) {
					txs = append(txs, &tx)
				}
			}

			if len(txs) == 0 {
				log.Net.Println("All transactions are invalid! Waiting for new ones...")
				return
			}

			cbTx := transaction.NewCoinbaseTX(miningAddress, "")
			txs = append(txs, cbTx)

			newBlock := miner.MineBlock(bc, txs)
			UTXOSet := blockchain.UTXOSet{bc}
			UTXOSet.Reindex()

			log.Net.Println("New block is mined!")

			for _, tx := range txs {
				txID := hex.EncodeToString(tx.ID)
				delete(mempool, txID)
			}

			for _, node := range knownNodes {
				sendInv(node, "block", [][]byte{newBlock.Hash})
			}

			if len(mempool) > 0 {
				goto MineTransactions
			}
		}
	}
}

/*
ping消息

The ping message is sent primarily to confirm that the TCP/IP connection is still valid.
*/
func (payload *ping) handleMsg(bc *blockchain.Blockchain, fromAddr string) {
	sendPong(fromAddr)
}

func (payload *addr) handleMsg(bc *blockchain.Blockchain, fromAddr string) {

}
