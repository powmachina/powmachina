package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// The job side of the library: a pool gets a template from the node, hands work to miners, and when
// a share meets the block target it has to send a **whole block** back. Between those two points
// sits every consensus byte there is — the coinbase with its mandatory dev fee output (SPEC §2.1),
// the merkle root, the 89-byte header layout, the transaction encoding.
//
// None of that is written again in C#. The pool calls in here, and what runs is `internal/work` —
// the same function the node's own stratum server uses, so a block from a pool and a block from
// solo mining are assembled by one piece of code. A pool that got the coinbase subtly wrong would
// find blocks, submit them, have them refused, and look to its miners like a pool with bad luck.
//
// The division of labour: consensus bytes and hashes are Go's, everything that is pool policy —
// share targets, vardiff, PPLNS, bans, payouts — stays in C# where it belongs.

type job struct {
	j      *work.Job
	params chain.NetParams
	payout []byte // the pool's own script, so the reward can be read off the coinbase
}

var (
	jobs       sync.Map // handle -> *job
	nextJob    uint64   = 1
	nextJobMu  sync.Mutex
	networkMap = map[string]chain.NetParams{
		"mainnet": chain.MainNet,
		"testnet": chain.TestNet,
		"regtest": chain.RegTest,
	}
)

// jobInfo is what the pool needs to know about a job it just had built. It is returned as JSON
// rather than through a dozen out-parameters: one marshalling point is easier to keep in step with
// the C# side than twelve pointer arguments, and this is called once per template, not per share.
type jobInfo struct {
	Handle      uint64 `json:"handle"`
	Height      uint64 `json:"height"`
	Lane        string `json:"lane"`
	Epoch       uint64 `json:"epoch"`
	PoW         string `json:"pow"`          // 80 B of header without the nonce, hex — the hashing input
	BlockTarget string `json:"block_target"` // 32 B big-endian, hex — at or below this it is a block
	Time        uint32 `json:"time"`         // the header time the coinbase was built with
}

// mh_job_build turns a gettemplate reply into a job. `templateJSON` is the node's reply verbatim
// (the `result` object), `payoutAddress` is where the pool pays itself, `network` is one of
// mainnet/testnet/regtest.
//
// It returns a NUL-terminated JSON string the caller must release with mh_free, or NULL on failure
// (then mh_last_error says why). The coinbase gets a fresh random extranonce on every call, so two
// jobs for the same address never hash the same input.
//
//export mh_job_build
func mh_job_build(templateJSON *C.char, payoutAddress *C.char, network *C.char) *C.char {
	if templateJSON == nil || payoutAddress == nil || network == nil {
		setErr("mh_job_build was given a null pointer")
		return nil
	}
	netName := C.GoString(network)
	params, ok := networkMap[netName]
	if !ok {
		setErr("unknown network " + netName + " (expected mainnet, testnet or regtest)")
		return nil
	}

	var t work.Template
	if err := json.Unmarshal([]byte(C.GoString(templateJSON)), &t); err != nil {
		setErr("the template is not readable JSON: " + err.Error())
		return nil
	}

	// The address is decoded here and not in C#: the pool must not be able to pay itself to
	// something the network will not accept, and the check for the script version and the key
	// length is the same one the miner makes.
	v, prog, err := address.Decode(params.Bech32HRP, C.GoString(payoutAddress))
	if err != nil {
		setErr("the payout address is not valid for " + netName + ": " + err.Error())
		return nil
	}
	if v != chain.ScriptVersionBasic || len(prog) != chain.PubkeyBytes {
		setErr("the payout address is not a basic " + netName + " address")
		return nil
	}

	j, err := work.Build(&t, prog, params)
	if err != nil {
		// work.ErrChainAhead is a wait, not a fault: the chain's time is ahead of this machine's
		// clock and a block built now would be refused. The pool should hold off, not alarm.
		setErr("the job could not be built: " + err.Error())
		return nil
	}

	nextJobMu.Lock()
	h := nextJob
	nextJob++
	nextJobMu.Unlock()
	jobs.Store(h, &job{j: j, params: params, payout: prog})

	info := jobInfo{
		Handle:      h,
		Height:      j.Height,
		Lane:        j.Lane.String(),
		Epoch:       j.Epoch,
		PoW:         hex.EncodeToString(j.PoW[:]),
		BlockTarget: hex.EncodeToString(j.BlockTarget[:]),
		Time:        j.Header.Time,
	}
	out, err := json.Marshal(info)
	if err != nil {
		jobs.Delete(h)
		setErr("the job description could not be marshalled: " + err.Error())
		return nil
	}
	return C.CString(string(out))
}

// mh_job_hash computes the PoW for a job and a nonce. It is a convenience over mh_hash: the pool
// does not have to keep the job's 80 bytes on the C# side, and the lane cannot be mismatched with
// the job it came from.
//
//export mh_job_hash
func mh_job_hash(ctxHandle C.ulonglong, jobHandle C.ulonglong, nonce C.ulonglong, out *C.uchar) C.int {
	if out == nil {
		setErr("out is a null pointer")
		return errBadArgs
	}
	v, ok := jobs.Load(uint64(jobHandle))
	if !ok {
		setErr("unknown job handle")
		return errNoContext
	}
	jb := v.(*job)
	lane := C.int(0)
	if jb.j.Lane == mh.LaneG {
		lane = 1
	}
	hdr := jb.j.PoW
	return mh_hash(ctxHandle, lane, (*C.uchar)(unsafe.Pointer(&hdr[0])), nonce, out)
}

// mh_job_block_hex assembles the whole block with the nonce found and returns it as hex, ready for
// the node's submitblock. The caller releases the string with mh_free.
//
//export mh_job_block_hex
func mh_job_block_hex(jobHandle C.ulonglong, nonce C.ulonglong) *C.char {
	v, ok := jobs.Load(uint64(jobHandle))
	if !ok {
		setErr("unknown job handle")
		return nil
	}
	jb := v.(*job)
	blk := jb.j.Block(uint64(nonce))
	return C.CString(hex.EncodeToString(blk.Serialize()))
}

// mh_job_id returns the block id (hash of the header) the assembled block would have — what the pool
// records as the block it found, and what an explorer shows. Hex, released with mh_free.
//
//export mh_job_id
func mh_job_id(jobHandle C.ulonglong, nonce C.ulonglong) *C.char {
	v, ok := jobs.Load(uint64(jobHandle))
	if !ok {
		setErr("unknown job handle")
		return nil
	}
	jb := v.(*job)
	h := jb.j.Header
	h.Nonce = uint64(nonce)
	id := h.ID()
	return C.CString(hex.EncodeToString(id[:]))
}

// mh_job_reward is what the coinbase pays **the pool** for this job, in cogs — read off the
// assembled coinbase by matching the pool's own output script, not recomputed from the subsidy. A
// pool that recomputes it gets the PPLNS arithmetic wrong on the day the dev fee schedule changes,
// and the error is silent: the shares are right, only the money is not.
//
//export mh_job_reward
func mh_job_reward(jobHandle C.ulonglong, out *C.longlong) C.int {
	if out == nil {
		setErr("out is a null pointer")
		return errBadArgs
	}
	v, ok := jobs.Load(uint64(jobHandle))
	if !ok {
		setErr("unknown job handle")
		return errNoContext
	}
	jb := v.(*job)
	if len(jb.j.Txs) == 0 {
		setErr("the job has no coinbase")
		return errHashFailed
	}
	var total int64
	for _, o := range jb.j.Txs[0].Outputs {
		if bytes.Equal(o.Script, jb.payout) {
			total += o.Value
		}
	}
	*out = C.longlong(total)
	return errOK
}

// mh_job_free releases a job. A pool keeps a window of recent jobs so a late share still counts, and
// drops them from the far end — without this the DAG-sized coinbase data of every job ever built
// would stay in memory.
//
//export mh_job_free
func mh_job_free(jobHandle C.ulonglong) {
	jobs.Delete(uint64(jobHandle))
}

// mh_address_valid says whether an address is a valid payout address on a network. A pool needs it
// at login: a miner that types a mainnet address into a testnet pool would otherwise mine happily
// into nothing and only find out when no payout ever arrives.
//
// It is here and not in the caller for the usual reason — bech32 with the right HRP, script version
// and key length is a small thing to get subtly wrong, and `internal/address` already gets it right.
// Returns 1 for valid, 0 otherwise; the reason is in mh_last_error.
//
//export mh_address_valid
func mh_address_valid(addr *C.char, network *C.char) C.int {
	if addr == nil || network == nil {
		setErr("mh_address_valid was given a null pointer")
		return 0
	}
	params, ok := networkMap[C.GoString(network)]
	if !ok {
		setErr("unknown network " + C.GoString(network))
		return 0
	}
	v, prog, err := address.Decode(params.Bech32HRP, C.GoString(addr))
	if err != nil {
		setErr("the address is not valid for " + params.Name + ": " + err.Error())
		return 0
	}
	if v != chain.ScriptVersionBasic || len(prog) != chain.PubkeyBytes {
		setErr("the address is not a basic " + params.Name + " address")
		return 0
	}
	return 1
}

// mh_network_info describes a network so the pool does not have to carry a second copy of the
// constants: the address prefix for validating what miners log in with, and the coinbase maturity
// the payout handler has to wait out. JSON, released with mh_free.
//
//export mh_network_info
func mh_network_info(network *C.char) *C.char {
	if network == nil {
		setErr("network is a null pointer")
		return nil
	}
	name := C.GoString(network)
	params, ok := networkMap[name]
	if !ok {
		setErr("unknown network " + name)
		return nil
	}
	out, err := json.Marshal(map[string]any{
		"name":              params.Name,
		"bech32_hrp":        params.Bech32HRP,
		"coinbase_maturity": chain.CoinbaseMaturity,
		"block_time_s":      int(chain.BlockTime.Seconds()),
		"lane_block_time_s": int(chain.LaneBlockTime.Seconds()),
	})
	if err != nil {
		setErr(fmt.Sprintf("network info could not be marshalled: %v", err))
		return nil
	}
	return C.CString(string(out))
}
