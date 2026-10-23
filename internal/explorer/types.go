package explorer

// The shapes of the node's answers.
//
// Every monetary value is an integer number of cogs (`*_cogs`), never a float. It is consensus rule 2
// carried all the way to the RPC boundary: in an earlier explorer an amount was read as a JSON float and
// diverged from the node. Formatting into POWM is done by the template.

type Info struct {
	NodeName     string   `json:"node_name"`
	Network      string   `json:"network"`
	Height       uint64   `json:"height"`
	Tip          string   `json:"tip"`
	TipTime      int64    `json:"tip_time"`
	ChainWork    string   `json:"chainwork"`
	UTXOCount    int      `json:"utxo_count"`
	MempoolTxs   int      `json:"mempool_txs"`
	MempoolBytes int      `json:"mempool_bytes"`
	AllowedLanes []string `json:"allowed_lanes"`
	Epoch        uint64   `json:"epoch"`
}

type BlockSummary struct {
	ID           string `json:"id"`
	Height       uint64 `json:"height"`
	Lane         string `json:"lane"`
	Time         int64  `json:"time"`
	Bits         string `json:"bits"`
	TxCount      int    `json:"tx_count"`
	Size         int    `json:"size"`
	ValueOutCogs int64  `json:"value_out_cogs"`
	// Body is false while the node has the header but not the body yet. The row is shown
	// anyway — a height that disappears from the list makes the two blocks around it look like
	// neighbours, which is how a valid chain can look like it has four blocks of one lane in a
	// row. A node that does not send the field at all is taken to have the body: a pointer
	// tells "no" apart from "did not say".
	Body *bool `json:"body"`
	// Payout is the address of the miner who closed the block, Shares how many shares it settles.
	Payout string `json:"payout"`
	Shares int    `json:"shares"`
}

// Pending says the node does not have this block's body yet, so there are no transactions and
// no size to show.
func (b BlockSummary) Pending() bool { return b.Body != nil && !*b.Body }

type BlockList struct {
	Height uint64         `json:"height"`
	Blocks []BlockSummary `json:"blocks"`
}

type Block struct {
	ID            string   `json:"id"`
	Height        uint64   `json:"height"`
	Lane          string   `json:"lane"`
	Time          int64    `json:"time"`
	Bits          string   `json:"bits"`
	Version       uint32   `json:"version"`
	MerkleRoot    string   `json:"merkle_root"`
	Nonce         uint64   `json:"nonce"`
	PrevBlock     string   `json:"prev_block"`
	NextBlock     string   `json:"next_block"`
	Work          string   `json:"work"`
	Confirmations uint64   `json:"confirmations"`
	Stale         bool     `json:"stale"`
	Size          int      `json:"size"`
	Txs           []string `json:"txs"`
	// Payout is who closed the block, ShareWeight the weight its own share claims.
	Payout      string `json:"payout"`
	ShareWeight uint8  `json:"share_weight"`
	// Settlement is who the shares the block settles belong to; nil = it settles none.
	Settlement *ShareTally `json:"settlement"`
}

// ShareTally is shares summed by miner (getblock's settlement, getsharepool).
type ShareTally struct {
	Shares     int          `json:"shares"`
	Units      uint64       `json:"units"`
	MinerCount int          `json:"miner_count"`
	Miners     []MinerShare `json:"miners"`
}

type MinerShare struct {
	Address string `json:"address"`
	Shares  int    `json:"shares"`
	Units   uint64 `json:"units"`
}

// SharePool is getsharepool: the shares the node holds for the next blocks.
type SharePool struct {
	ShareTally
	TakesShares bool `json:"takes_shares"`
}

type VIn struct {
	Coinbase  bool   `json:"coinbase"`
	TxID      string `json:"txid"`
	Vout      uint32 `json:"vout"`
	ValueCogs int64  `json:"value_cogs"`
	Address   string `json:"address"`
}

type VOut struct {
	N             int    `json:"n"`
	ValueCogs     int64  `json:"value_cogs"`
	ScriptVersion uint8  `json:"script_version"`
	Script        string `json:"script"`
	Address       string `json:"address"`
}

type Tx struct {
	TxID          string `json:"txid"`
	Block         string `json:"block"`
	Height        uint64 `json:"height"`
	Index         uint32 `json:"index"`
	Time          int64  `json:"time"`
	Lane          string `json:"lane"`
	Confirmations uint64 `json:"confirmations"`
	Coinbase      bool   `json:"coinbase"`
	VIn           []VIn  `json:"vin"`
	VOut          []VOut `json:"vout"`
	ValueInCogs   int64  `json:"value_in_cogs"`
	ValueOutCogs  int64  `json:"value_out_cogs"`
	FeeCogs       int64  `json:"fee_cogs"`
	Size          int    `json:"size"`
}

type AddrTx struct {
	TxID      string `json:"txid"`
	Height    uint64 `json:"height"`
	NetCogs   int64  `json:"net_cogs"`
	InCogs    int64  `json:"in_cogs"`
	OutCogs   int64  `json:"out_cogs"`
	Direction string `json:"direction"`
}

type Address struct {
	Address      string   `json:"address"`
	BalanceCogs  int64    `json:"balance_cogs"`
	ReceivedCogs int64    `json:"received_cogs"`
	SentCogs     int64    `json:"sent_cogs"`
	TxCount      uint64   `json:"tx_count"`
	Txs          []AddrTx `json:"txs"`
}

type RichEntry struct {
	Rank          int    `json:"rank"`
	Address       string `json:"address"`
	BalanceCogs   int64  `json:"balance_cogs"`
	SharePermille int64  `json:"share_permille"`
}

type RichList struct {
	SupplyCogs int64       `json:"supply_cogs"`
	Addresses  []RichEntry `json:"addresses"`
}

type LaneShare struct {
	Blocks        int   `json:"blocks"`
	OfBlocks      int   `json:"of_blocks"`
	SharePermille int64 `json:"share_permille"`
}

type Lane struct {
	Lane            string               `json:"lane"`
	Bits            string               `json:"bits"`
	Share           map[string]LaneShare `json:"share"`
	TargetSpacingS  int64                `json:"target_spacing_s"`
	AvgSpacingS     int64                `json:"avg_spacing_s"`
	HashrateEst     string               `json:"hashrate_est"`
	WorkPerBlock    string               `json:"work_per_block"`
	LastBlockHeight uint64               `json:"last_block_height"`
	LastBlockTime   int64                `json:"last_block_time"`
}

type LaneRun struct {
	Lane           string `json:"lane"`
	Length         int    `json:"length"`
	MaxSameLaneRun int    `json:"max_same_lane_run"`
}

type LaneStats struct {
	Height  uint64  `json:"height"`
	Windows []int   `json:"windows"`
	Lanes   []Lane  `json:"lanes"`
	Run     LaneRun `json:"run"`
}

type Finality struct {
	Finalized bool   `json:"finalized"`
	Height    uint64 `json:"height"`
	Block     string `json:"block"`
}
