// Copyright (c) 2014-2016 Bitmark Inc.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
)

// global constants
const (
	bitcoinMinimumVersion = 90200 // do not start if bitcoind older than this
	totalTries            = 5     // retry failed connections
)

// errors
var (
	//ErrNotInitialised          = errors.New("not initialised")
	ErrInvalidBitcoinVersion   = errors.New("invalid bitcoin version")
	ErrInvalidBitcoinChain     = errors.New("invalid bitcoin chain")
	ErrInvalidMethod           = errors.New("invalid method")
	ErrTooFewArguments         = errors.New("too few arguments")
	ErrTooManyArguments        = errors.New("too many arguments")
	ErrInvalidArgumentType     = errors.New("invalid argument type")
	ErrRpcError                = errors.New("RPC error")
	ErrIncomprehesibleResponse = errors.New("incomprehesible response")
	ErrHexLengthIncorrect      = errors.New("hex length incorrect")
	ErrInvalidBool             = errors.New("invalid bool: 0/1 expected")
	ErrAccessDenied            = errors.New("Access denied")
)

// RPC request
type Call struct {
	Method    string
	Arguments []json.RawMessage
	Response  chan interface{}
	Tries     int
}

// globals for background proccess
type RemoteConnection struct {
	sync.RWMutex // to allow locking

	// connection to bitcoin daemon
	client *http.Client
	url    string

	// authentication
	username string
	password string

	// identifier for the RPC
	id uint64

	// current height
	latestBlockNumber uint64

	// for the background
	shutdown chan bool
	finished chan bool
}

// shared queue
var sharedQueue = make(chan Call)

// external API
// ------------

// connet to a either bitcoind or a miniature-spoon proxy
func NewRemoteConnection(url string, username string, password string, chain string, tls *tls.Config) (*RemoteConnection, error) {

	conn := RemoteConnection{
		id:       0,
		username: username,
		password: password,
		url:      url,

		client: &http.Client{},

		shutdown: make(chan bool),
		finished: make(chan bool),
	}

	if nil != tls {
		conn.client.Transport = &http.Transport{
			TLSClientConfig: tls,
		}
	}

	// query bitcoind for blockchain status
	// only need to have necessary fields as JSON unmarshaller will ignore excess
	var blockchainReply struct {
		Chain string `json:"chain"`
		//Blocks uint64 `json:"blocks"`
	}
	var rpcErr interface{}
	err := conn.remoteCall("getblockchaininfo", []interface{}{}, &blockchainReply, &rpcErr)
	if nil != err {
		return nil, err
	}
	if chain != blockchainReply.Chain {
		return nil, ErrInvalidBitcoinChain
	}

	// query bitcoind for general status
	// only need to have necessary fields as JSON unmarshaller will ignore excess
	var infoReply struct {
		Version uint64 `json:"version"`
		Blocks  uint64 `json:"blocks"`
	}
	err = conn.remoteCall("getinfo", []interface{}{}, &infoReply, &rpcErr)
	if nil != err {
		return nil, err
	}

	// check version is sufficient
	if infoReply.Version < bitcoinMinimumVersion {
		return nil, ErrInvalidBitcoinVersion
	}

	// set up current block number
	conn.latestBlockNumber = infoReply.Blocks

	// start background processes
	go conn.background(sharedQueue)

	return &conn, nil
}

// finialise - stop all background tasks
func (conn *RemoteConnection) Destroy() {

	// stop background
	close(conn.shutdown)

	// wait for stop
	<-conn.finished
}

// some types for RPC results
type RawError json.RawMessage
type RawResult json.RawMessage

// to ensure null works correctly
var jsonNull = json.RawMessage("null")

// the main RPC calling routine
func RemoteCall(method string, arguments []json.RawMessage) (json.RawMessage, json.RawMessage, error) {
	r := make(chan interface{})
	c := Call{
		Method:    method,
		Arguments: arguments,
		Response:  r,
	}

	tries := totalTries
	for {
		tries -= 1

		// send request
		sharedQueue <- c

		// receive response
		result := <-r

		//decode the result
		switch result.(type) {
		case error:
			if tries <= 1 {
				return jsonNull, jsonNull, result.(error)
			}
		case RawResult:
			return json.RawMessage(result.(RawResult)), jsonNull, nil
		case RawError:
			return jsonNull, json.RawMessage(result.(RawError)), nil
		default:
			return jsonNull, jsonNull, ErrIncomprehesibleResponse
		}
	}
}

// background process
func (conn *RemoteConnection) background(queue <-chan Call) {

loop:
	for {
		select {
		case <-conn.shutdown:
			break loop
		case call := <-queue:

			var reply json.RawMessage
			var rpcerr json.RawMessage

			//log.Printf("dequeued call: %v\n", call)
			err := conn.processCall(call.Method, call.Arguments, &reply, &rpcerr)

			//log.Printf("pc: reply: %v\n", reply)
			//log.Printf("pc: reply: %s\n", reply)
			//log.Printf("pc: rpcerr: %v\n", rpcerr)
			//log.Printf("pc: rpcerr: %s\n", rpcerr)

			if nil != rpcerr {
				call.Response <- RawError(rpcerr)
			} else if nil != err {
				call.Response <- err
			} else {
				call.Response <- RawResult(reply)
			}

		}
	}
	close(conn.finished)
}

// check if a parameter element is a valid hash string, if so extract it
func getHex(argument json.RawMessage, size int) (string, error) {

	var hexData string
	err := json.Unmarshal(argument, &hexData)
	if nil != err {
		return "", ErrInvalidArgumentType
	}
	bytes, err := hex.DecodeString(hexData)
	if nil != err {
		return "", err
	}
	if size > 0 && len(bytes) != size {
		return "", ErrHexLengthIncorrect
	}
	return hexData, nil
}

// check if a parameter is a number, if so extract it
func getNumber(argument json.RawMessage) (uint64, error) {
	var number uint64
	err := json.Unmarshal(argument, &number)
	if nil != err {
		return 0, ErrInvalidArgumentType
	}
	return number, nil
}

// process only allowable RPCs
func (conn *RemoteConnection) processCall(method string, arguments []json.RawMessage, reply *json.RawMessage, rpcErr *json.RawMessage) error {

	count := len(arguments)

	switch method {

	case "getinfo":
		if 0 != count {
			return ErrTooManyArguments
		}
		return conn.remoteCall("getinfo", []interface{}{}, reply, rpcErr)

	case "getblockchaininfo":
		if 0 != count {
			return ErrTooManyArguments
		}
		return conn.remoteCall("getblockchaininfo", []interface{}{}, reply, rpcErr)

	case "getblockcount":
		if 0 != count {
			return ErrTooManyArguments
		}
		return conn.remoteCall("getblockcount", []interface{}{}, reply, rpcErr)

	case "getblockhash":
		if count < 1 {
			return ErrTooFewArguments
		} else if count > 1 {
			return ErrTooManyArguments
		}

		number, err := getNumber(arguments[0])
		if nil != err {
			return err
		}

		return conn.remoteCall("getblockhash", []interface{}{number}, reply, rpcErr)

	case "getblock":
		if count < 1 {
			return ErrTooFewArguments
		} else if count > 1 {
			return ErrTooManyArguments
		}

		hash, err := getHex(arguments[0], 32)
		if nil != err {
			return err
		}

		return conn.remoteCall("getblock", []interface{}{hash}, reply, rpcErr)

	case "getrawtransaction":

		if count < 1 {
			return ErrTooFewArguments
		} else if count > 2 {
			return ErrTooManyArguments
		}

		hash, err := getHex(arguments[0], 32)
		if nil != err {
			return err
		}
		number := uint64(0) // optional
		if count >= 2 {
			number, err = getNumber(arguments[1])
			if nil != err {
				return err
			}
			if number < 0 || number > 1 {
				return ErrInvalidBool
			}
		}

		return conn.remoteCall("getrawtransaction", []interface{}{hash, number}, reply, rpcErr)

	case "decoderawtransaction":
		if count < 1 {
			return ErrTooFewArguments
		} else if count > 1 {
			return ErrTooManyArguments
		}

		hexData, err := getHex(arguments[0], 0)
		if nil != err {
			return err
		}

		return conn.remoteCall("decoderawtransaction", []interface{}{hexData}, reply, rpcErr)

	case "sendrawtransaction":
		if count < 1 {
			return ErrTooFewArguments
		} else if count > 1 {
			return ErrTooManyArguments
		}

		hexData, err := getHex(arguments[0], 0)
		if nil != err {
			return err
		}

		return conn.remoteCall("sendrawtransaction", []interface{}{hexData}, reply, rpcErr)

	default:
		return ErrInvalidMethod
	}
}

// low level RPC
// -------------

// high level call - only use while global data locked
// because the HTTP RPC cannot interleave calls and responses
func (conn *RemoteConnection) remoteCall(method string, params []interface{}, reply interface{}, rpcerr interface{}) error {

	conn.id += 1

	arguments := bitcoinArguments{
		ID:         conn.id,
		Method:     method,
		Parameters: params,
	}
	response := bitcoinReply{
		Result: reply,
		Error:  rpcerr,
	}
	//log.Printf("arguments: %v\n", arguments)
	err := conn.bitcoinRPC(&arguments, &response)
	//log.Printf("response: %v\n", response)
	//log.Printf("reply: %v\n", reply)
	if nil != err {
		return err
	}
	return nil
}

// for encoding the RPC arguments
type bitcoinArguments struct {
	ID         uint64        `json:"id"`
	Method     string        `json:"method"`
	Parameters []interface{} `json:"params"`
}

// for decoding the RPC reply
type bitcoinReply struct {
	Id     int64       `json:"id"`
	Method string      `json:"method"`
	Result interface{} `json:"result"`
	Error  interface{} `json:"error"`
}

// basic RPC - only use while global data locked
func (conn *RemoteConnection) bitcoinRPC(arguments *bitcoinArguments, reply *bitcoinReply) error {

	s, err := json.Marshal(arguments)
	if nil != err {
		return err
	}

	postData := bytes.NewBuffer(s)

	request, err := http.NewRequest(http.MethodPost, conn.url, postData)
	if nil != err {
		return err
	}
	request.SetBasicAuth(conn.username, conn.password)

	response, err := conn.client.Do(request)
	if nil != err {
		return err
	}
	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if nil != err {
		return err
	}

	if http.StatusOK == response.StatusCode {
		err = json.Unmarshal(body, &reply)
		if nil != err {
			return err
		}
		return nil
	}
	if http.StatusUnauthorized == response.StatusCode {
		return ErrAccessDenied
	}
	return fmt.Errorf("http failed: %q", response.Status)
}
