package proxy

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/yuriy0803/open-etc-pool-friends/util"
)

const (
	MaxReqSize = 1024
)

const (
	EthProxy int = iota
	NiceHash
)

func (s *ProxyServer) ListenTCP() {
	// Parse timeout duration from configuration
	s.timeout = util.MustParseDuration(s.config.Proxy.Stratum.Timeout)

	var err error
	var server net.Listener

	// If TLS is enabled, load certificate and key file and create a TLS listener
	if s.config.Proxy.Stratum.TLS {
		var cert tls.Certificate
		cert, err = tls.LoadX509KeyPair(s.config.Proxy.Stratum.CertFile, s.config.Proxy.Stratum.KeyFile)
		if err != nil {
			log.Fatalln("Error loading certificate:", err)
		}
		tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
		server, err = tls.Listen("tcp", s.config.Proxy.Stratum.Listen, tlsCfg)
	} else {
		// Otherwise, create a regular TCP listener
		server, err = net.Listen("tcp", s.config.Proxy.Stratum.Listen)
	}
	if err != nil {
		log.Fatalf("Error: %v", err)
	}
	defer server.Close()

	log.Printf("Stratum listening on %s", s.config.Proxy.Stratum.Listen)
	var accept = make(chan int, s.config.Proxy.Stratum.MaxConn)
	n := 0

	for {
		// Accept new incoming connections
		conn, err := server.Accept()
		if err != nil {
			continue
		}
		ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())

		// Apply IP banning and connection limiting policies
		if s.policy.IsBanned(ip) || !s.policy.ApplyLimitPolicy(ip) {
			conn.Close()
			continue
		}
		n += 1
		// Generate a unique extranonce value for this session
		extranonce := s.uniqExtranonce()
		cs := &Session{conn: conn, ip: ip, Extranonce: extranonce, ExtranonceSub: false, stratum: -1}
		// Allocate a stale jobs cache for this session
		cs.staleJobs = make(map[string]staleJob)

		accept <- n
		// Start a new goroutine to handle the session
		go func(cs *Session) {
			err = s.handleTCPClient(cs)
			if err != nil {
				s.removeSession(cs)
				conn.Close()
			}
			<-accept
		}(cs)
	}
}

// handleTCPClient reads incoming data from a client and handles it appropriately.
func (s *ProxyServer) handleTCPClient(cs *Session) error {
	// Create an encoder to send data to the client
	cs.enc = json.NewEncoder(cs.conn)

	// Create a buffer to read incoming data from the client
	connbuff := bufio.NewReaderSize(cs.conn, MaxReqSize)

	// Set a deadline for the connection
	s.setDeadline(cs.conn)

	for {
		// Read a line of data from the client
		data, isPrefix, err := connbuff.ReadLine()

		// If the data is too large for the buffer, ban the client and return an error
		if isPrefix {
			log.Printf("Socket flood detected from %s", cs.ip)
			s.policy.BanClient(cs.ip)
			return err
		}

		// If the client disconnects, remove their session and break the loop
		if err == io.EOF {
			log.Printf("Client %s disconnected", cs.ip)
			s.removeSession(cs)
			break
		}

		// If there was an error reading from the client, log the error and return it
		if err != nil {
			log.Printf("Error reading from socket: %v", err)
			return err
		}

		// If the data is not empty, attempt to unmarshal it as a Stratum request
		if len(data) > 1 {
			var req StratumReq
			err = json.Unmarshal(data, &req)

			// If the data is malformed, apply the malformed policy, log the error, and return it
			if err != nil {
				s.policy.ApplyMalformedPolicy(cs.ip)
				log.Printf("Malformed stratum request from %s: %v", cs.ip, err)
				return err
			}

			// Set a new deadline for the connection
			s.setDeadline(cs.conn)

			// Handle the incoming message from the client
			err = cs.handleTCPMessage(s, &req)

			// If there was an error handling the message, return it
			if err != nil {
				return err
			}
		}
	}

	// Return nil when finished handling the client
	return nil
}

// setStratumMode sets the stratum mode of the session based on the provided string.
// If the string is "EthereumStratum/1.0.0", the stratum mode will be set to NiceHash.
// Otherwise, the stratum mode will be set to EthProxy.
// Returns an error if the string is empty or if an invalid stratum mode is provided.
func (cs *Session) setStratumMode(str string) error {
	switch str {
	case "EthereumStratum/1.0.0":
		cs.stratum = NiceHash
	default:
		cs.stratum = EthProxy
	}
	return nil
}

// stratumMode returns the current stratum mode of the session.
// The returned value is an integer representing the current stratum mode,
// where 0 represents EthProxy and 1 represents NiceHash.
func (cs *Session) stratumMode() int {
	// Returns the current stratum mode of the session.
	return cs.stratum
}

func (cs *Session) handleTCPMessage(s *ProxyServer, req *StratumReq) error {
	// Handle RPC/Stratum methods
	switch req.Method {
	// claymore -esm 1
	case "eth_login":
		// Unmarshal request parameters
		var params []string
		err := json.Unmarshal(req.Params, &params)
		if err != nil {
			log.Println("Malformed stratum request params from", cs.ip)
			return err
		}
		// Process "eth_login" method and return response
		reply, errReply := s.handleLoginRPC(cs, params, req.Worker)
		if errReply != nil {
			return cs.sendTCPError(req.Id, errReply)
		}
		return cs.sendTCPResult(req.Id, reply)
		// claymore -esm 0
	case "eth_submitLogin":
		var params []string
		if err := json.Unmarshal(req.Params, &params); err != nil || len(params) < 1 {
			log.Printf("Malformed stratum request params from %s: %v", cs.ip, err)
			return err
		}
		reply, err := s.handleLoginRPC(cs, params, req.Worker)
		if err != nil {
			log.Printf("Error handling login RPC from %s: %v", cs.ip, err)
			return cs.sendTCPError(req.Id, err)
		}
		cs.setStratumMode("EthProxy")
		log.Printf("EthProxy login from %s: %v", cs.ip, params[0])
		return cs.sendTCPResult(req.Id, reply)

	case "mining.subscribe":
		params := []string{}
		if err := json.Unmarshal(req.Params, &params); err != nil || len(params) < 2 {
			log.Println("Malformed stratum request params from", cs.ip, err)
			return err
		}

		if params[1] != "EthereumStratum/1.0.0" && params[0] != "GodMiner/2.0.0" {
			log.Println("Unsupported stratum version from", cs.ip)
			return cs.sendStratumError(req.Id, "unsupported stratum version")
		}

		cs.ExtranonceSub = true
		cs.setStratumMode("EthereumStratum/1.0.0")
		log.Println("Nicehash subscribe", cs.ip)

		result := cs.getNotificationResponse(s)
		if err := cs.sendStratumResult(req.Id, result); err != nil {
			log.Println("Failed to send stratum result:", err)
			return err
		}

		return nil

	default:
		switch cs.stratumMode() {
		case 0:
			break
		case 1:
			break
		default:
			errReply := s.handleUnknownRPC(cs, req.Method)
			return cs.sendTCPError(req.Id, errReply)
		}
	}

	if cs.stratumMode() == NiceHash {
		switch req.Method {
		case "mining.authorize":
			var params []string
			err := json.Unmarshal(req.Params, &params)
			if err != nil || len(params) < 1 {
				return errors.New("invalid params")
			}
			splitData := strings.Split(params[0], ".")
			params[0] = splitData[0]
			reply, errReply := s.handleLoginRPC(cs, params, req.Worker)
			if errReply != nil {
				return cs.sendStratumError(req.Id, []string{
					string(errReply.Code),
					errReply.Message,
				})
			}

			if err := cs.sendStratumResult(req.Id, reply); err != nil {
				return err
			}

			paramsDiff := []float64{
				util.DiffIntToFloat(s.config.Proxy.Difficulty),
			}
			respReq := JSONStratumReq{Method: "mining.set_difficulty", Params: paramsDiff}
			if err := cs.sendTCPReq(respReq); err != nil {
				return err
			}

			return cs.sendJob(s, req.Id, true)

		case "mining.extranonce.subscribe":
			var params []string
			err := json.Unmarshal(req.Params, &params)
			if err != nil {
				return errors.New("invalid params")
			}
			if len(params) == 0 {
				if err := cs.sendStratumResult(req.Id, true); err != nil {
					return err
				}
				cs.ExtranonceSub = true
				req := JSONStratumReq{
					Id:     nil,
					Method: "mining.set_extranonce",
					Params: []interface{}{
						cs.Extranonce,
					},
				}
				return cs.sendTCPReq(req)
			}
			return cs.sendStratumError(req.Id, []string{
				"20",
				"Not supported.",
			})
		case "mining.submit":
			var params []string
			err := json.Unmarshal(req.Params, &params)
			if err != nil || len(params) < 3 {
				log.Println("mining.submit: json.Unmarshal fail")
				return err
			}

			// params[0] = Username
			// params[1] = Job ID
			// params[2] = Minernonce
			// Reference:
			// https://github.com/nicehash/nhethpool/blob/060817a9e646cd9f1092647b870ed625ee138ab4/nhethpool/EthereumInstance.cs#L369

			// WORKER NAME MANDATORY  0x1234.WORKERNAME
			splitData := strings.Split(params[0], ".")
			id := "0"
			if len(splitData) > 1 {
				id = splitData[1]
			}

			cs.worker = id

			// check Extranonce subscription.
			extranonce := cs.Extranonce
			if !cs.ExtranonceSub {
				extranonce = ""
			}
			nonce := extranonce + params[2]

			if cs.JobDetails.JobID != params[1] {
				stale, ok := cs.staleJobs[params[1]]
				if ok {
					log.Printf("Cached stale JobID %s", params[1])
					params = []string{
						nonce,
						stale.SeedHash,
						stale.HeaderHash,
					}
				} else {
					log.Printf("Stale share (mining.submit JobID received %s != current %s)", params[1], cs.JobDetails.JobID)
					if err := cs.sendStratumError(req.Id, []string{"21", "Stale share."}); err != nil {
						return err
					}
					return cs.sendJob(s, req.Id, false)
				}
			} else {
				params = []string{
					nonce,
					cs.JobDetails.SeedHash,
					cs.JobDetails.HeaderHash,
				}
			}

			reply, errReply := s.handleTCPSubmitRPC(cs, id, params)
			if errReply != nil {
				log.Println("mining.submit: handleTCPSubmitRPC failed")
				return cs.sendStratumError(req.Id, []string{
					strconv.Itoa(errReply.Code),
					errReply.Message,
				})
			}

			// TEST, ein notify zu viel
			//if err := cs.sendTCPResult(resp); err != nil {
			//	return err
			//}

			//return cs.sendJob(s, req.Id)
			return cs.sendStratumResult(req.Id, reply)

		default:
			errReply := s.handleUnknownRPC(cs, req.Method)
			return cs.sendStratumError(req.Id, []string{
				strconv.Itoa(errReply.Code),
				errReply.Message,
			})
		}
	}

	switch req.Method {
	case "eth_getWork":
		reply, errReply := s.handleGetWorkRPC(cs)
		if errReply != nil {
			return cs.sendTCPError(req.Id, errReply)
		}
		return cs.sendTCPResult(req.Id, &reply)
	// Handle requests of type "eth_submitWork"
	case "eth_submitWork":
		// Unmarshal the parameters from the request into a slice of strings
		var params []string
		err := json.Unmarshal(req.Params, &params)
		// Check if there was an error unmarshaling the parameters or if they don't meet the required length and format criteria
		if err != nil || len(params) < 3 || len(params[0]) != 18 || len(params[1]) != 66 || len(params[2]) != 66 {
			// If there was an error, log the issue and return it
			log.Println("Malformed stratum request params from", cs.ip)
			return err
		}
		// If the parameters are valid, call the handler function for submitting work
		reply, errReply := s.handleTCPSubmitRPC(cs, req.Worker, params)
		// Check if there was an error handling the request
		if errReply != nil {
			// If there was, return the error
			return cs.sendTCPError(req.Id, errReply)
		}
		// If the request was handled successfully, return the result
		return cs.sendTCPResult(req.Id, &reply)

	case "eth_submitHashrate":
		var params []string
		err := json.Unmarshal(req.Params, &params)
		if err != nil || len(params) < 2 {
			log.Println("Malformed stratum request params from", cs.ip)
			return err
		}

		hashrateStr := params[0]
		if !strings.HasPrefix(hashrateStr, "0x") {
			log.Println("Malformed hashrate value in eth_submitHashrate request from", cs.ip)
			return errors.New("Malformed hashrate value")
		}

		hashrate, err := strconv.ParseInt(hashrateStr[2:], 16, 64)
		if err != nil {
			log.Println("Malformed hashrate value in eth_submitHashrate request from", cs.ip)
			return err
		}

		formattedHashrate := formatEthHashrate(hashrate)
		log.Printf("Hashrate reported by %v@%v (%v): %s", cs.worker, cs.ip, cs.login, formattedHashrate)
		return cs.sendTCPResult(req.Id, true)
	default:
		errReply := s.handleUnknownRPC(cs, req.Method)
		return cs.sendTCPError(req.Id, errReply)
	}
}

func formatEthHashrate(shareDiffCalc int64) string {
	units := []string{"H/s", "KH/s", "MH/s", "GH/s", "TH/s", "PH/s"}
	var i int
	diff := float64(shareDiffCalc)

	for i = 0; i < len(units)-1 && diff >= 1000.0; i++ {
		diff /= 1000.0
	}

	formatted := strconv.FormatFloat(diff, 'f', 2, 64)
	return formatted + " " + units[i]
}

func (cs *Session) sendTCPResult(id json.RawMessage, result interface{}) error {
	cs.Lock()
	defer cs.Unlock()

	message := JSONRpcResp{Id: id, Version: "2.0", Error: nil, Result: result}
	return cs.enc.Encode(&message)
}

// cache stale jobs
func (cs *Session) cacheStales(max, n int) {
	l := len(cs.staleJobIDs)
	// remove outdated stales except last n caches if l > max
	if l > max {
		save := cs.staleJobIDs[l-n : l]
		del := cs.staleJobIDs[0 : l-n]
		for _, v := range del {
			delete(cs.staleJobs, v)
		}
		cs.staleJobIDs = save
	}
	// save stales cache
	cs.staleJobs[cs.JobDetails.JobID] = staleJob{
		cs.JobDetails.SeedHash,
		cs.JobDetails.HeaderHash,
	}
	cs.staleJobIDs = append(cs.staleJobIDs, cs.JobDetails.JobID)
}

func (cs *Session) pushNewJob(s *ProxyServer, result interface{}) error {
	cs.Lock()
	defer cs.Unlock()

	if cs.stratumMode() == NiceHash {
		cs.cacheStales(10, 3)

		t := result.(*[]string)
		cs.JobDetails = jobDetails{
			JobID:      randomHex(8),
			SeedHash:   (*t)[1],
			HeaderHash: (*t)[0],
			Height:     (*t)[3],
		}

		// strip 0x prefix
		if cs.JobDetails.SeedHash[0:2] == "0x" {
			cs.JobDetails.SeedHash = cs.JobDetails.SeedHash[2:]
			cs.JobDetails.HeaderHash = cs.JobDetails.HeaderHash[2:]
		}

		a := s.currentBlockTemplate()

		resp := JSONStratumReq{
			Method: "mining.notify",
			Params: []interface{}{
				cs.JobDetails.JobID,
				cs.JobDetails.SeedHash,
				cs.JobDetails.HeaderHash,
				// If set to true, then miner needs to clear queue of jobs and immediatelly
				// start working on new provided job, because all old jobs shares will
				// result with stale share error.
				//
				// if true, NiceHash charges "Extra Rewards" for frequent job changes
				// if false, the stale rate might be higher because miners take too long to switch jobs
				//
				// It's undetermined what's more cost-effective
				false,
			},

			Height: util.ToHex1(int64(a.Height)),
			Algo:   s.config.Algo,
		}
		return cs.enc.Encode(&resp)
	}
	// FIXME: Temporarily add ID for Claymore compliance
	message := JSONPushMessage{Version: "2.0", Result: result, Id: 0}
	return cs.enc.Encode(&message)
}

func (cs *Session) sendTCPError(id json.RawMessage, reply *ErrorReply) error {
	cs.Lock()
	defer cs.Unlock()

	message := JSONRpcResp{Id: id, Version: "2.0", Error: reply}
	err := cs.enc.Encode(&message)
	if err != nil {
		return err
	}
	return errors.New(reply.Message)
}

func (cs *Session) sendStratumResult(id json.RawMessage, result interface{}) error {
	cs.Lock()
	defer cs.Unlock()

	resp := JSONRpcResp{Id: id, Error: nil, Result: result}
	return cs.enc.Encode(&resp)
}

func (cs *Session) sendStratumError(id json.RawMessage, message interface{}) error {
	cs.Lock()
	defer cs.Unlock()

	resp := JSONRpcResp{Id: id, Error: message}

	return cs.enc.Encode(&resp)
}

func (cs *Session) sendTCPReq(resp JSONStratumReq) error {
	cs.Lock()
	defer cs.Unlock()

	return cs.enc.Encode(&resp)
}

func (self *ProxyServer) setDeadline(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(self.timeout))
}

func (s *ProxyServer) registerSession(cs *Session) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.sessions[cs] = struct{}{}
}

func (s *ProxyServer) removeSession(cs *Session) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	delete(s.Extranonces, cs.Extranonce)
	delete(s.sessions, cs)
}

// nicehash
func (cs *Session) sendJob(s *ProxyServer, id json.RawMessage, newjob bool) error {
	if newjob {
		reply, errReply := s.handleGetWorkRPC(cs)
		if errReply != nil {
			return cs.sendStratumError(id, []string{
				string(errReply.Code),
				errReply.Message,
			})
		}

		cs.JobDetails = jobDetails{
			JobID:      randomHex(8),
			SeedHash:   reply[1],
			HeaderHash: reply[0],
			Height:     reply[3],
		}

		// The NiceHash official .NET pool omits 0x...
		// TO DO: clean up once everything works
		if cs.JobDetails.SeedHash[0:2] == "0x" {
			cs.JobDetails.SeedHash = cs.JobDetails.SeedHash[2:]
			cs.JobDetails.HeaderHash = cs.JobDetails.HeaderHash[2:]
		}
	}

	t := s.currentBlockTemplate()

	resp := JSONStratumReq{
		Method: "mining.notify",
		Params: []interface{}{
			cs.JobDetails.JobID,
			cs.JobDetails.SeedHash,
			cs.JobDetails.HeaderHash,
			true,
		},

		Height: util.ToHex1(int64(t.Height)),
		Algo:   s.config.Algo,
	}

	return cs.sendTCPReq(resp)
}

func (s *ProxyServer) broadcastNewJobs() {
	t := s.currentBlockTemplate()
	if t == nil || len(t.Header) == 0 || s.isSick() {
		return
	}
	reply := []string{t.Header, t.Seed, s.diff, util.ToHex(int64(t.Height))}

	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()

	count := len(s.sessions)
	log.Printf("Broadcasting new job to %v stratum miners", count)

	start := time.Now()
	bcast := make(chan int, 1024)
	n := 0

	for m, _ := range s.sessions {
		n++
		bcast <- n

		go func(cs *Session) {
			err := cs.pushNewJob(s, &reply)
			<-bcast
			if err != nil {
				log.Printf("Job transmit error to %v@%v: %v", cs.login, cs.ip, err)
				s.removeSession(cs)
			} else {
				s.setDeadline(cs.conn)
			}
		}(m)
	}
	log.Printf("Jobs broadcast finished %s", time.Since(start))
}

func (s *ProxyServer) uniqExtranonce() string {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()

	extranonce := randomHex(4)
	for {
		if _, ok := s.Extranonces[extranonce]; ok {
			extranonce = randomHex(4)
		} else {
			break
		}
	}
	s.Extranonces[extranonce] = true
	return extranonce
}

func randomHex(strlen int) string {
	rand.Seed(time.Now().UTC().UnixNano())
	const chars = "0123456789abcdef"
	result := make([]byte, strlen)
	for i := 0; i < strlen; i++ {
		result[i] = chars[rand.Intn(len(chars))]
	}
	return string(result)
}

func (cs *Session) getNotificationResponse(s *ProxyServer) interface{} {
	result := make([]interface{}, 2)
	result[0] = []string{"mining.notify", randomHex(16), "EthereumStratum/1.0.0"}
	result[1] = cs.Extranonce

	// Additional response data for NiceHash stratum mode
	if cs.stratumMode() == NiceHash {
		t := s.currentBlockTemplate()
		if t != nil {
			// Construct the response for NiceHash "mining.notify"
			jobID := randomHex(8)
			seedHash := t.Seed
			headerHash := t.Header
			height := util.ToHex1(int64(t.Height))
			// TO DO: Clean up the response structure based on actual data in "t"

			result = []interface{}{
				"mining.notify",
				jobID,
				seedHash,
				headerHash,
				height,
			}
		}
	}

	return result
}
